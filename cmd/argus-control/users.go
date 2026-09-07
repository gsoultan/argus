package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
