package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/apk/apktest"
)

/*
An app run on a device: the file is fetched, checked against what the
console showed, and only then handed to Android; a no from the person
holding the device is a failed run, not a retry.
*/

type fakeInstaller struct {
	mu    sync.Mutex
	calls []string
	err   error
	read  []byte
}

func (f *fakeInstaller) note(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}
func (f *fakeInstaller) Install(path, pkg string) error {
	b, _ := readFile(path)
	f.mu.Lock()
	f.read = b
	f.mu.Unlock()
	return f.note("install " + pkg)
}
func (f *fakeInstaller) Uninstall(pkg string) error { return f.note("uninstall " + pkg) }
func (f *fakeInstaller) Disable(pkg string) error   { return f.note("disable " + pkg) }
func (f *fakeInstaller) Restore(pkg string) error   { return f.note("restore " + pkg) }

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func waitRun(t *testing.T, r *AppRuns) PackageRunStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Status(); s != nil && s.Finished() {
			return *s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the run did not end")
	return PackageRunStatus{}
}

func installOrder(id string, file []byte, cert []byte, code int64) PackageRunRequest {
	return PackageRunRequest{ID: id, Mode: AppInstall, App: &AppOrder{Package: "com.impack.survey", File: &AppFile{
		Path: "/api/v1/report/software-file/" + id, SHA256: apktest.SHA256(file), Size: int64(len(file)),
		VersionCode: code, VersionName: "2.4.0", Signer: apktest.SHA256(cert)}}}
}

func TestAnInstallHandsAndroidOnlyTheFileTheConsoleShowed(t *testing.T) {
	file, cert := apktest.Signed(t, "com.impack.survey", 240, "2.4.0", "Field Survey")
	serve := file
	inst := &fakeInstaller{}
	var asked []string
	r := &AppRuns{Dir: t.TempDir(), Installer: inst, Attended: func() bool { return true },
		Fetch: func(_ context.Context, path string, w io.Writer, limit int64) (int64, error) {
			asked = append(asked, path)
			n, err := io.Copy(w, io.LimitReader(bytes.NewReader(serve), limit+1))
			return n, err
		}}
	done := make(chan struct{})
	r.Start(context.Background(), installOrder("11111111-aaaa", file, cert, 240), func() { close(done) })
	<-done
	st := waitRun(t, r)
	if st.State != RunDone || len(inst.calls) != 1 || inst.calls[0] != "install com.impack.survey" ||
		!bytes.Equal(inst.read, file) || asked[0] != "/api/v1/report/software-file/11111111-aaaa" {
		t.Fatalf("%+v %v", st, inst.calls)
	}
	if len(st.Upgraded) != 1 || st.Upgraded[0].To != "2.4.0" {
		t.Errorf("changes %+v", st.Upgraded)
	}
	// The same order again is the same run, not a second install.
	r.Start(context.Background(), installOrder("11111111-aaaa", file, cert, 240), nil)
	time.Sleep(20 * time.Millisecond)
	if len(inst.calls) != 1 {
		t.Errorf("installed twice: %v", inst.calls)
	}

	other, otherCert := apktest.Signed(t, "com.impack.other", 240, "2.4.0", "")
	for name, c := range map[string]struct {
		serve []byte
		order PackageRunRequest
		want  string
	}{
		"a changed file":  {append(append([]byte{}, file...), 0), installOrder("22222222-bbbb", file, cert, 240), "SHA-256 differs"},
		"another app":     {other, installOrder("33333333-cccc", other, otherCert, 240), "is com.impack.other, not com.impack.survey"},
		"another version": {file, installOrder("44444444-dddd", file, cert, 241), "version code 240, not 241"},
		"another signer":  {file, installOrder("55555555-eeee", file, otherCert, 240), "signed by a different key"},
	} {
		inst.calls = nil
		serve = c.serve
		r.Start(context.Background(), c.order, nil)
		st := waitRun(t, r)
		if st.State != RunFailed || !strings.Contains(st.Message, c.want) || len(inst.calls) != 0 {
			t.Errorf("%s: %+v, Android was asked %v", name, st, inst.calls)
		}
	}
}

func TestAPersonsNoIsAFailedRun(t *testing.T) {
	inst := &fakeInstaller{err: errors.New("the person holding the device said no")}
	r := &AppRuns{Dir: t.TempDir(), Installer: inst, Attended: func() bool { return true }}
	r.Start(context.Background(), PackageRunRequest{ID: "66666666-ffff", Mode: AppUninstall,
		App: &AppOrder{Package: "com.impack.survey"}}, nil)
	st := waitRun(t, r)
	if st.State != RunFailed || !strings.Contains(st.Message, "said no") {
		t.Errorf("%+v", st)
	}
	for _, bad := range []PackageRunRequest{
		{ID: "77777777-0000", Mode: AppUninstall, App: &AppOrder{Package: "x; reboot"}},
		{ID: "88888888-0000", Mode: AppInstall, App: &AppOrder{Package: "com.impack.survey"}},
		{ID: "99999999-0000", Mode: "all", App: &AppOrder{Package: "com.impack.survey"}},
	} {
		inst.calls = nil
		r.Start(context.Background(), bad, nil)
		if st := waitRun(t, r); st.State != RunFailed || len(inst.calls) != 0 {
			t.Errorf("%+v: %+v %v", bad, st, inst.calls)
		}
	}
}
