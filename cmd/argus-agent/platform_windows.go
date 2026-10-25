package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/gsoultan/argus/internal/agent"
)

// The sshd hook is not ported: direct logins are not recorded on Windows.
const shimSupported = false

const serviceName = "argus-agent"

// isPrivileged is true for LocalSystem and an elevated administrator.
func isPrivileged() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

const privilegedName = "an administrator (an elevated prompt)"

// serviceStop is closed when the service manager asks the daemon to stop.
var serviceStop = make(chan struct{})

func stopRequests() <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	stop := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-serviceStop:
		}
		close(stop)
	}()
	return stop
}

// runAsService runs the daemon under the service manager when it started
// this process, which it does with "daemon" as the only argument.
func runAsService() (int, bool) {
	ok, err := svc.IsWindowsService()
	if err != nil || !ok {
		return 0, false
	}
	// A service has no console: what the daemon logs goes to a file beside
	// its state, which only SYSTEM and Administrators can read.
	if err := agent.ProtectStateDir(agent.DefaultStateDir); err == nil {
		if f, err := openLog(filepath.Join(agent.DefaultStateDir, "agent.log")); err == nil {
			os.Stdout, os.Stderr = f, f
		}
	}
	h := &service{args: os.Args[2:]}
	if err := svc.Run(serviceName, h); err != nil {
		return 1, true
	}
	return h.code, true
}

// openLog appends to path, starting it afresh past 10 MB.
func openLog(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

type service struct {
	args []string
	code int
}

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest,
	status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	done := make(chan int, 1)
	go func() { done <- runDaemon(s.args) }()
	status <- svc.Status{State: svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
	for {
		select {
		case s.code = <-done:
			return false, uint32(s.code)
		case r := <-req:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				// Sealing open recordings can take a while: say so, or the
				// service manager gives up on us.
				status <- svc.Status{State: svc.StopPending, WaitHint: 70000}
				close(serviceStop)
				s.code = <-done
				return false, uint32(s.code)
			}
		}
	}
}

func runService(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: argus-agent service install|uninstall|start|stop")
		return 2
	}
	if !isPrivileged() {
		fmt.Fprintln(os.Stderr, "argus-agent service: run as "+privilegedName)
		return 1
	}
	var err error
	switch args[0] {
	case "install":
		err = installService(args[1:])
	case "uninstall":
		err = uninstallService()
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	default:
		fmt.Fprintln(os.Stderr, "usage: argus-agent service install|uninstall|start|stop")
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-agent service %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// installService registers the agent to start at boot as LocalSystem, the
// account that can log others on, and starts it. Arguments after "install"
// are passed to the daemon.
func installService(daemonArgs []string) error {
	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	noStart := fs.Bool("no-start", false, "register the service without starting it")
	_ = fs.Parse(daemonArgs)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return errors.New("already installed; uninstall it first")
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "Argus agent",
		Description: "Records sessions on this host and holds its tunnel to the Argus control plane.",
		StartType:   mgr.StartAutomatic,
	}, append([]string{"daemon"}, fs.Args()...)...)
	if err != nil {
		return err
	}
	defer s.Close()
	// Restarted when it fails, as systemd's Restart=on-failure does.
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	// Time to seal open recordings when the host shuts down, as launchd's
	// ExitTimeOut and systemd's TimeoutStopSec give.
	pre := struct{ Timeout uint32 }{Timeout: 75000}
	_ = windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO,
		(*byte)(unsafe.Pointer(&pre)))
	fmt.Println("installed the argus-agent service")
	if *noStart {
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("installed, but it did not start: %w", err)
	}
	fmt.Println("started it")
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("not installed")
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	return s.Delete()
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("not installed; run argus-agent service install")
	}
	defer s.Close()
	return s.Start()
}

func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("not installed")
	}
	defer s.Close()
	_, err = s.Control(svc.Stop)
	return err
}
