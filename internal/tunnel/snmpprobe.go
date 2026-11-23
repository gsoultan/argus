package tunnel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

/*
Reading a device over SNMP: who it says it is, how long it has been up, and
its interfaces. Only standard MIB-II and IF-MIB values are asked for, so it
works on any device without anyone typing an OID.
*/

const (
	oidSysDescr    = ".1.3.6.1.2.1.1.1.0"
	oidSysUpTime   = ".1.3.6.1.2.1.1.3.0"
	oidSysName     = ".1.3.6.1.2.1.1.5.0"
	oidSysLocation = ".1.3.6.1.2.1.1.6.0"

	oidIfDescr       = ".1.3.6.1.2.1.2.2.1.2"
	oidIfSpeed       = ".1.3.6.1.2.1.2.2.1.5"
	oidIfAdminStatus = ".1.3.6.1.2.1.2.2.1.7"
	oidIfOperStatus  = ".1.3.6.1.2.1.2.2.1.8"
	oidIfInOctets    = ".1.3.6.1.2.1.2.2.1.10"
	oidIfInErrors    = ".1.3.6.1.2.1.2.2.1.14"
	oidIfOutOctets   = ".1.3.6.1.2.1.2.2.1.16"
	oidIfOutErrors   = ".1.3.6.1.2.1.2.2.1.20"
	oidIfName        = ".1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCInOctets  = ".1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOctets = ".1.3.6.1.2.1.31.1.1.1.10"
	oidIfHighSpeed   = ".1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias       = ".1.3.6.1.2.1.31.1.1.1.18"
)

// SNMPProbe reads devices. Its zero value uses the network.
type SNMPProbe struct {
	// Dial stands in for a device in tests: given the query, it answers.
	Dial func(ctx context.Context, q SNMPQuery) (*SNMPAnswer, string, error)
	// DialSystem is Dial for a system-group-only read (discovery); it
	// stands in for a device in tests the same way.
	DialSystem func(ctx context.Context, q SNMPQuery) (*SNMPAnswer, string, error)
}

// Discover tries each community string in order against the device and
// returns the first that answers, read-only. It reads only the system
// group: enough to prove a string works and to show who the device is.
func (p SNMPProbe) Discover(ctx context.Context, d SNMPDiscover) SNMPDiscovery {
	res := SNMPDiscovery{}
	if err := d.Validate(); err != nil {
		res.Detail = err.Error()
		return res
	}
	read := p.DialSystem
	if read == nil {
		read = readSNMPSystem
	}
	var detail string
	for _, c := range d.Communities {
		if ctx.Err() != nil {
			break
		}
		q := SNMPQuery{Host: d.Host, Port: d.Port, Version: SNMPv2c, Community: c, TimeoutSec: d.TimeoutSec}
		res.Tried++
		a, fail, err := read(ctx, q)
		if err == nil {
			res.Found, res.Community, res.SNMP = true, c, a
			return res
		}
		detail = err.Error()
		// A name that does not resolve or an address no check may reach
		// fails the same way for every string: stop and say so once.
		if fail == SNMPFailAddress || fail == SNMPFailBlocked {
			break
		}
	}
	res.Detail = detail
	return res
}

// Probe asks the device once and says whether it passes.
func (p SNMPProbe) Probe(ctx context.Context, q SNMPQuery) AttemptResult {
	res := AttemptResult{Kind: CheckKindSNMP}
	if err := q.Validate(); err != nil {
		res.Fail, res.Detail = SNMPFailBlocked, err.Error()
		return res
	}
	start := time.Now()
	read := p.Dial
	if read == nil {
		read = readSNMP
	}
	a, fail, err := read(ctx, q)
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		res.Fail, res.Detail = fail, err.Error()
		return res
	}
	res.SNMP = a
	res.OK, res.Detail = SNMPVerdict(q, a)
	if !res.OK {
		res.Fail = SNMPFailInterface
	}
	return res
}

func snmpClient(ctx context.Context, q SNMPQuery, addr string) *gosnmp.GoSNMP {
	timeout := q.TimeoutSec
	if timeout == 0 {
		timeout = SNMPTimeoutDefault
	}
	port := q.Port
	if port == 0 {
		port = 161
	}
	g := &gosnmp.GoSNMP{
		Target:         addr,
		Port:           uint16(port),
		Context:        ctx,
		Timeout:        time.Duration(timeout) * time.Second / 2,
		Retries:        1,
		MaxOids:        gosnmp.MaxOids,
		MaxRepetitions: 25,
	}
	if q.Version == SNMPv2c {
		g.Version, g.Community = gosnmp.Version2c, q.Community
		return g
	}
	g.Version = gosnmp.Version3
	g.SecurityModel = gosnmp.UserSecurityModel
	usm := &gosnmp.UsmSecurityParameters{UserName: q.User, AuthenticationProtocol: gosnmp.NoAuth, PrivacyProtocol: gosnmp.NoPriv}
	g.MsgFlags = gosnmp.NoAuthNoPriv
	if q.AuthProto != "" {
		usm.AuthenticationProtocol = map[string]gosnmp.SnmpV3AuthProtocol{
			"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA224": gosnmp.SHA224, "SHA256": gosnmp.SHA256, "SHA384": gosnmp.SHA384, "SHA512": gosnmp.SHA512,
		}[q.AuthProto]
		usm.AuthenticationPassphrase = q.AuthPass
		g.MsgFlags = gosnmp.AuthNoPriv
	}
	if q.PrivProto != "" {
		usm.PrivacyProtocol = map[string]gosnmp.SnmpV3PrivProtocol{
			"DES": gosnmp.DES, "AES": gosnmp.AES, "AES192": gosnmp.AES192, "AES256": gosnmp.AES256,
		}[q.PrivProto]
		usm.PrivacyPassphrase = q.PrivPass
		g.MsgFlags = gosnmp.AuthPriv
	}
	g.SecurityParameters = usm
	return g
}

// readSNMPSystem reads only the device's system group over the network:
// its name, description, uptime and location. Discovery uses it to prove a
// community string works without the slower interface walk.
func readSNMPSystem(ctx context.Context, q SNMPQuery) (*SNMPAnswer, string, error) {
	addr, fail, err := snmpResolve(ctx, q.Host)
	if err != nil {
		return nil, fail, err
	}
	g := snmpClient(ctx, q, addr)
	if err := g.Connect(); err != nil {
		return nil, SNMPFailNoAnswer, fmt.Errorf("could not open a socket to %s: %v", q.Host, err)
	}
	defer g.Conn.Close()
	pkt, err := g.Get([]string{oidSysName, oidSysDescr, oidSysUpTime, oidSysLocation})
	if err != nil {
		return nil, snmpFail(err), snmpErr(q, err)
	}
	if pkt.Error != gosnmp.NoError {
		return nil, SNMPFailRefused, fmt.Errorf("%s refused: %s", q.Host, pkt.Error)
	}
	a := &SNMPAnswer{Interfaces: []SNMPInterface{}}
	for _, v := range pkt.Variables {
		switch v.Name {
		case oidSysName:
			a.Name = snmpString(v)
		case oidSysDescr:
			a.Description = snmpString(v)
		case oidSysLocation:
			a.Location = snmpString(v)
		case oidSysUpTime:
			if n := gosnmp.ToBigInt(v.Value); n != nil {
				a.UptimeSec = n.Int64() / 100
			}
		}
	}
	if a.Name == "" && a.Description == "" && a.UptimeSec == 0 {
		return nil, SNMPFailRefused, fmt.Errorf("%s answered but shared nothing: its SNMP view may not include the system group", q.Host)
	}
	return a, "", nil
}

// readSNMP reads the device over the network.
func readSNMP(ctx context.Context, q SNMPQuery) (*SNMPAnswer, string, error) {
	addr, fail, err := snmpResolve(ctx, q.Host)
	if err != nil {
		return nil, fail, err
	}
	g := snmpClient(ctx, q, addr)
	if err := g.Connect(); err != nil {
		return nil, SNMPFailNoAnswer, fmt.Errorf("could not open a socket to %s: %v", q.Host, err)
	}
	defer g.Conn.Close()

	pkt, err := g.Get([]string{oidSysName, oidSysDescr, oidSysUpTime, oidSysLocation})
	if err != nil {
		return nil, snmpFail(err), snmpErr(q, err)
	}
	if pkt.Error != gosnmp.NoError {
		return nil, SNMPFailRefused, fmt.Errorf("%s refused: %s", q.Host, pkt.Error)
	}
	a := &SNMPAnswer{Interfaces: []SNMPInterface{}}
	for _, v := range pkt.Variables {
		switch v.Name {
		case oidSysName:
			a.Name = snmpString(v)
		case oidSysDescr:
			a.Description = snmpString(v)
		case oidSysLocation:
			a.Location = snmpString(v)
		case oidSysUpTime:
			if n := gosnmp.ToBigInt(v.Value); n != nil {
				a.UptimeSec = n.Int64() / 100
			}
		}
	}
	if a.Name == "" && a.Description == "" && a.UptimeSec == 0 {
		return nil, SNMPFailRefused, fmt.Errorf("%s answered but shared nothing: its SNMP view may not include the system group", q.Host)
	}

	ifs := map[int]*SNMPInterface{}
	at := func(idx int) *SNMPInterface {
		i := ifs[idx]
		if i == nil {
			i = &SNMPInterface{Index: idx}
			ifs[idx] = i
		}
		return i
	}
	walk := func(root string, set func(*SNMPInterface, gosnmp.SnmpPDU)) error {
		var pdus []gosnmp.SnmpPDU
		var err error
		if g.Version == gosnmp.Version1 {
			pdus, err = g.WalkAll(root)
		} else {
			pdus, err = g.BulkWalkAll(root)
		}
		if err != nil {
			return err
		}
		for _, v := range pdus {
			idx, err := strconv.Atoi(v.Name[strings.LastIndexByte(v.Name, '.')+1:])
			if err != nil || idx <= 0 {
				continue
			}
			if _, ok := ifs[idx]; !ok && len(ifs) >= SNMPInterfacesMax {
				continue
			}
			set(at(idx), v)
		}
		return nil
	}
	num := func(v gosnmp.SnmpPDU) uint64 {
		if n := gosnmp.ToBigInt(v.Value); n != nil && n.Sign() >= 0 {
			return n.Uint64()
		}
		return 0
	}
	// The interface table is the part that matters; IF-MIB's extensions
	// (names, 64-bit counters, aliases) are read when the device has them.
	if err := walk(oidIfDescr, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.Name = snmpString(v) }); err != nil {
		return nil, snmpFail(err), snmpErr(q, err)
	}
	steps := []struct {
		oid string
		set func(*SNMPInterface, gosnmp.SnmpPDU)
	}{
		{oidIfOperStatus, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.Up = num(v) == 1 }},
		{oidIfAdminStatus, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.AdminUp = num(v) == 1 }},
		{oidIfSpeed, func(i *SNMPInterface, v gosnmp.SnmpPDU) {
			if i.SpeedMbps == 0 {
				i.SpeedMbps = num(v) / 1_000_000
			}
		}},
		{oidIfInOctets, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.InOctets = num(v) }},
		{oidIfOutOctets, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.OutOctets = num(v) }},
		{oidIfInErrors, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.InErrors = num(v) }},
		{oidIfOutErrors, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.OutErrors = num(v) }},
		{oidIfName, func(i *SNMPInterface, v gosnmp.SnmpPDU) {
			if s := snmpString(v); s != "" {
				i.Name = s
			}
		}},
		{oidIfAlias, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.Alias = snmpString(v) }},
		{oidIfHighSpeed, func(i *SNMPInterface, v gosnmp.SnmpPDU) {
			if n := num(v); n > 0 {
				i.SpeedMbps = n
			}
		}},
		{oidIfHCInOctets, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.InOctets = num(v) }},
		{oidIfHCOutOctets, func(i *SNMPInterface, v gosnmp.SnmpPDU) { i.OutOctets = num(v) }},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return nil, SNMPFailNoAnswer, fmt.Errorf("%s stopped answering part way", q.Host)
		}
		// A device without IF-MIB's extensions answers "no such object";
		// what it has is still worth having.
		_ = walk(s.oid, s.set)
	}
	for _, i := range ifs {
		a.Interfaces = append(a.Interfaces, *i)
	}
	sort.Slice(a.Interfaces, func(x, y int) bool { return a.Interfaces[x].Index < a.Interfaces[y].Index })
	return a, "", nil
}

func snmpString(v gosnmp.SnmpPDU) string {
	switch s := v.Value.(type) {
	case []byte:
		return strings.TrimSpace(strings.ToValidUTF8(string(s), "?"))
	case string:
		return strings.TrimSpace(s)
	}
	return ""
}

func snmpFail(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "timed out"), errors.Is(err, context.DeadlineExceeded):
		return SNMPFailNoAnswer
	case strings.Contains(msg, "refused"):
		return SNMPFailNoAnswer
	}
	return SNMPFailRefused
}

func snmpErr(q SNMPQuery, err error) error {
	if snmpFail(err) == SNMPFailNoAnswer {
		port := q.Port
		if port == 0 {
			port = 161
		}
		return fmt.Errorf("%s did not answer SNMP on port %d", q.Host, port)
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "unknown user"), strings.Contains(low, "authentication"), strings.Contains(low, "wrong digest"), strings.Contains(low, "decrypt"):
		return fmt.Errorf("%s refused the credentials: check the user, protocols and passwords", q.Host)
	}
	return fmt.Errorf("%s answered with an error: %s", q.Host, msg)
}
