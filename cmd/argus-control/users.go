package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/gsoultan/argus/internal/control"
)

// `argus-control users add` creates the first account.
//
// Every deployment needs one person who can sign in before anyone can be
// invited, and a product that seeds a default administrator with a known
// password has shipped a backdoor. So the first account is made deliberately,
// on the host, by someone who already has shell access there -- which is a
// privilege boundary that already exists rather than a new one.
//
// The password is read from the terminal without echo, so it does not land in
// shell history, in a process list, or in whatever ships the logs.
func runUsersAdd(configPath string, args []string) error {
	email := ""
	role := "admin"
	name := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--role":
			if i+1 >= len(args) {
				return errors.New("--role needs a value")
			}
			i++
			role = args[i]
		case "--name":
			if i+1 >= len(args) {
				return errors.New("--name needs a value")
			}
			i++
			name = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q", args[i])
			}
			email = args[i]
		}
	}
	if email == "" {
		return errors.New("usage: argus-control users add <email> [--role admin|owner|approver|operator|auditor] [--name \"Full Name\"]")
	}
	switch role {
	case "owner", "admin", "approver", "operator", "auditor":
	default:
		return fmt.Errorf("unknown role %q", role)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	password, err := readPassword()
	if err != nil {
		return err
	}

	if _, err := store.CreateAccount(ctx, email, name, role, password); err != nil {
		return err
	}
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action: "user.created", Severity: "notice", ActorEmail: "console",
		Target: email,
		Detail: "Local account created from the host with role " + role + ".",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: account created but not audited: %v\n", err)
	}

	fmt.Printf("\nCreated %s as %s.\n\n", email, role)
	fmt.Printf("  Sign in at the console and enrol a second factor straight away —\n")
	fmt.Printf("  a privileged account with a password and nothing else is one phish\n")
	fmt.Printf("  away from every host Argus brokers.\n\n")
	return nil
}

// readPassword prompts twice, without echo.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped input: read one line and use it. Useful for provisioning, and
		// the operator has chosen to take responsibility for where it came from.
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Print("Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Print("Repeat:   ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match")
	}
	return string(first), nil
}

// `argus-control users list` shows who can sign in, and who used to.
//
// Disabled accounts are listed rather than hidden: an operator asking who has
// access needs to see that an account exists and is revoked, not to be shown a
// list it is missing from and conclude it was never there.
func runUsersList(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	accounts, err := store.Accounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("No accounts. Create one with: argus-control users add <email>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tROLE\tMFA\tSTATE")
	for _, a := range accounts {
		mfa := "no"
		if a.MFAEnrolled {
			mfa = "yes"
		}
		state := "active"
		if a.Disabled {
			state = "disabled"
			if a.DisabledAt != nil {
				state += " " + a.DisabledAt.UTC().Format(time.RFC3339)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Email, a.Role, mfa, state)
	}
	return w.Flush()
}

// `argus-control users disable` and `users enable` revoke and restore access.
//
// The column these set has been read since it was added -- a disabled account
// cannot sign in, does not count toward accountsExist, and does not count as an
// administrator -- and nothing ever wrote it. An account could be created and
// never revoked, which for a product about controlling access is the wrong half
// of the pair to ship.
//
// On the host, like `users add`, and for the same reason: whoever has shell
// there can already reach the database, so this is not a new privilege. What it
// is, is a supported way to do it that leaves an audit entry, instead of an
// UPDATE somebody types from memory at the wrong moment.
func runUsersSetDisabled(configPath string, args []string, disabled bool) error {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: argus-control users %s <email>", verb)
	}
	email := args[0]

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	switch err := store.SetAccountDisabled(ctx, email, disabled); {
	case err == nil:
	case errors.Is(err, control.ErrLastAdmin):
		return fmt.Errorf("refusing to disable %s: %w\n"+
			"Give another account the admin or owner role first. A deployment "+
			"nobody can administer is recovered with direct SQL, which is what "+
			"this command exists to avoid needing.", email, err)
	case errors.Is(err, control.ErrAlreadyInState):
		fmt.Printf("%s is already %sd.\n", email, verb)
		return nil
	case errors.Is(err, control.ErrNoAccount):
		return fmt.Errorf("no account for %s", email)
	default:
		return err
	}

	// Written down, because revoking access is exactly the kind of act an
	// investigation asks about afterwards. The actor is the host user: this
	// runs without a console session, and naming the shell that did it is more
	// honest than attributing it to the service.
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action:     "account." + verb + "d",
		Severity:   "notice",
		ActorEmail: hostActor(),
		Target:     email,
		Detail: fmt.Sprintf("Account %s %sd with argus-control on the host.",
			email, verb),
	}); err != nil {
		// The account is already changed; failing here would report the
		// opposite of what happened.
		fmt.Fprintf(os.Stderr, "warning: %sd %s but could not write the audit entry: %v\n",
			verb, email, err)
	}

	fmt.Printf("%sd %s.\n", verb, email)
	return nil
}

// hostActor names whoever ran this, preferring the human behind a sudo.
func hostActor() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u + "@host"
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username + "@host"
	}
	return "unknown@host"
}
