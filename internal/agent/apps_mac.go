package agent

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

/*
The macOS app scan, as pure functions of what it reads, so they are tested
on any platform. macOS has no /proc: lsof names the process behind each
listening socket, ps lists what runs, and Homebrew's Cellar holds what was
installed, one directory per formula and one per version under it.
*/

// parseLsofListen reads `lsof -nP -iTCP -sTCP:LISTEN -F pcn`: a p line
// starts a process, c is its command, each n is one of its listening
// addresses as host:port, with * for every address.
func parseLsofListen(out string) []Listener {
	var res []Listener
	seen := map[ListenAddr]bool{}
	pid, cmd := 0, ""
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
			cmd = ""
		case 'c':
			cmd = v
		case 'n':
			host, port, err := net.SplitHostPort(v)
			if err != nil {
				continue
			}
			p, err := strconv.Atoi(port)
			if err != nil || p <= 0 || p > 65535 {
				continue
			}
			if host == "*" {
				host = "0.0.0.0"
			}
			key := ListenAddr{Address: host, Port: p}
			if seen[key] || len(res) >= maxListeners {
				continue
			}
			seen[key] = true
			res = append(res, Listener{Address: host, Port: p, Process: cmd, pid: pid})
		}
	}
	return res
}

// maxListeners bounds what one scan reports.
const maxListeners = 512

// catalogueNames keeps the names the catalogue knows, once each, from a
// list of process names such as `ps -axco comm` prints.
func catalogueNames(out string) []string {
	seen := map[string]bool{}
	res := []string{}
	for _, l := range strings.Split(out, "\n") {
		name := strings.TrimSpace(l)
		if name == "" || seen[name] || kindByProcess(name) == "" {
			continue
		}
		seen[name] = true
		res = append(res, name)
	}
	sort.Strings(res)
	return res
}

// brewCellar lists the formulae in a Homebrew Cellar with their newest
// installed version. postgresql@17 is reported as postgresql, the name the
// catalogue matches; the version keeps the 17.
func brewCellar(cellar string) ([]installedPackage, bool) {
	ents, err := os.ReadDir(cellar)
	if err != nil {
		return nil, false
	}
	var res []installedPackage
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		vers, err := os.ReadDir(filepath.Join(cellar, e.Name()))
		if err != nil {
			continue
		}
		var newest string
		for _, v := range vers {
			if v.IsDir() && !strings.HasPrefix(v.Name(), ".") && v.Name() > newest {
				newest = v.Name()
			}
		}
		if newest == "" {
			continue
		}
		name, _, _ := strings.Cut(e.Name(), "@")
		res = append(res, installedPackage{name: name, version: newest})
	}
	return res, true
}
