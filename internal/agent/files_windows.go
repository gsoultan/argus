package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
File transfer on Windows, as on Linux: each operation runs `argus-agent
file-op` as the account, so NTFS permissions decide what it may read and
write, as they would for the person signed in at the machine.

The account is logged on as the terminal does it (an S4U logon, see
logon_windows.go) once per files stream, its profile loaded so its home
exists, and each helper is started with that token on a desktop of its own,
in a job object, with no handles but its three pipes.
*/

// Files is the files service.
func Files(cfg FilesConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused file transfer", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(FilesService) {
			log.Warn("tunnel: refused file transfer on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for file transfer"}.Error())
			return
		}
		token, err := s4uToken(g.Principal)
		if err != nil {
			call.Refuse(err.Error())
			return
		}
		defer token.Close()
		user, _, _ := splitAccount(g.Principal)
		profile, err := loadProfile(token, user)
		if err != nil {
			call.Refuse(fmt.Sprintf("cannot load the profile of %s: %v", user, err))
			return
		}
		defer unloadProfile(token, profile)
		home, err := token.GetUserProfileDirectory()
		if err != nil {
			call.Refuse(fmt.Sprintf("cannot find the home folder of %s: %v", user, err))
			return
		}
		desktop, err := newSessionDesktop(token)
		if err != nil {
			call.Refuse(fmt.Sprintf("cannot prepare a desktop for %s: %v", user, err))
			return
		}
		defer desktop.Close()
		helper := cfg.Helper
		if helper == "" {
			if helper, err = os.Executable(); err != nil {
				call.Refuse(err.Error())
				return
			}
		}
		if call.Accept("") != nil {
			_ = call.Stream.Close()
			return
		}
		log.Info("tunnel: file transfer opened", "principal", user, "for", g.Email,
			"client", g.ClientAddr)
		f := &fileSession{who: user, home: home, actor: g.Email, log: log}
		f.run = func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
			return runAsToken(ctx, tokenRun{token: token, desktop: desktop.Name, home: home,
				helper: helper, args: append([]string{"file-op"}, args...), actor: g.Email,
				env: cfg.HelperEnv, stdin: stdin, stdout: stdout})
		}
		f.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

// tokenRun is one run of this executable as the account a token is for.
type tokenRun struct {
	token   windows.Token
	desktop string
	home    string
	helper  string
	args    []string
	actor   string
	env     []string
	stdin   io.Reader
	stdout  io.Writer
}

// runAsToken starts the helper as r.token and waits for it, copying stdin in
// and stdout out. The context ending ends it, and everything it started.
func runAsToken(ctx context.Context, r tokenRun) error {
	env, err := r.token.Environ(false)
	if err != nil {
		return err
	}
	env = append(append(env, "ARGUS_SESSION_FOR="+r.actor), r.env...)

	// Three pipes; the child's ends inheritable, ours not.
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	var inR, inW, outR, outW, errR, errW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, sa, 0); err != nil {
		return err
	}
	if err := windows.CreatePipe(&outR, &outW, sa, 1<<16); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return err
	}
	if err := windows.CreatePipe(&errR, &errW, sa, 0); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return err
	}
	childEnds := []windows.Handle{inR, outW, errW}
	ours := []windows.Handle{inW, outR, errR}
	closeChild := func() {
		for _, h := range childEnds {
			windows.CloseHandle(h)
		}
	}
	for _, h := range ours {
		_ = windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0)
	}
	inFile := os.NewFile(uintptr(inW), "file-op-in")
	outFile := os.NewFile(uintptr(outR), "file-op-out")
	errFile := os.NewFile(uintptr(errR), "file-op-err")
	defer inFile.Close()
	defer outFile.Close()
	defer errFile.Close()

	// Only the three pipe ends are inherited, whatever else in this process
	// happens to be inheritable.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeChild()
		return err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childEnds[0]),
		uintptr(len(childEnds))*unsafe.Sizeof(childEnds[0])); err != nil {
		closeChild()
		return err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = windows.StringToUTF16Ptr(r.desktop)
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = inR, outW, errW

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		closeChild()
		return err
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		closeChild()
		return err
	}

	cmdline := windows.ComposeCommandLine(append([]string{r.helper}, r.args...))
	var pi windows.ProcessInformation
	err = windows.CreateProcessAsUser(r.token, nil, windows.StringToUTF16Ptr(cmdline), nil, nil, true,
		windows.CREATE_SUSPENDED|windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_NO_WINDOW|
			windows.CREATE_UNICODE_ENVIRONMENT,
		envBlock(env), windows.StringToUTF16Ptr(r.home), &si.StartupInfo, &pi)
	closeChild()
	if err != nil {
		return fmt.Errorf("starting the file helper: %w", err)
	}
	defer windows.CloseHandle(pi.Process)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 255)
		windows.CloseHandle(pi.Thread)
		return err
	}
	_, _ = windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)

	var wg sync.WaitGroup
	var stderr bytes.Buffer
	var copyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, copyErr = io.Copy(r.stdout, outFile)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&stderr, io.LimitReader(errFile, 64<<10))
	}()
	if r.stdin != nil {
		go func() {
			_, _ = io.Copy(inFile, r.stdin)
			_ = inFile.Close()
		}()
	} else {
		_ = inFile.Close()
	}

	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = windows.TerminateJobObject(job, 1)
		case <-exited:
		}
	}()
	_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	close(exited)
	wg.Wait()
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(lastLine(msg))
		}
		return fmt.Errorf("the helper failed (exit %d)", code)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return copyErr
}
