package admin

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/tokens"
)

// tokens runs `fairy admin tokens create|list|revoke`.
func (c *cli) tokens(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	store := tokens.NewStore(c.cfg.DataDir)
	switch args[0] {
	case "create":
		return c.tokensCreate(store, args[1:])
	case "list":
		return c.tokensList(store)
	case "revoke":
		return c.tokensRevoke(store, args)
	}
	return 2, errUsage
}

// tokensCreate creates an API token and prints it once (only its hash is stored).
func (c *cli) tokensCreate(store *tokens.Store, args []string) (int, error) {
	fs := flag.NewFlagSet("tokens create", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	name := fs.String("name", "", "token name (1-64 chars)")
	owner := fs.String("owner", "", `an OIDC sub, or "system"`)
	scope := fs.String("scope", "read", "read or control")
	days := fs.Int("expires-days", 0, "expiry in days (0 = never)")
	if err := fs.Parse(args); err != nil {
		return 2, errUsage
	}
	if *name == "" || *owner == "" {
		return 2, errors.New("--name and --owner are required")
	}
	raw, r, err := store.Create(*name, *owner, *scope, "cli:"+osUser(), *days)
	if err != nil {
		return 2, err
	}
	var exp any
	expStr := "never"
	if r.ExpiresAt != nil {
		exp = r.ExpiresAt.Format(time.RFC3339)
		expStr = r.ExpiresAt.Format("2006-01-02")
	}
	c.log("token.create", r.Name, map[string]any{"id": r.ID, "owner": r.Owner, "scope": r.Scope, "expires_at": exp})
	fmt.Fprintf(c.out, "Created API token %q (id %s, scope %s, owner %s, expires %s)\n\n    %s\n\n", r.Name, r.ID, r.Scope, r.Owner, expStr, raw)
	fmt.Fprintln(c.out, "This is the only time the token is shown; only its SHA-256 hash is stored.")
	fmt.Fprintf(c.out, "Use it as:  curl -H 'Authorization: Bearer %s…' %s/api/summary\n", raw[:7], c.cfg.BaseURL())
	return 0, nil
}

// tokensList prints every token with its state (active, revoked, expired); never the token itself.
func (c *cli) tokensList(store *tokens.Store) (int, error) {
	var rows [][]string
	for _, r := range store.List() {
		state := "active"
		if r.RevokedAt != nil {
			state = "revoked"
		} else if !r.Active() {
			state = "expired"
		}
		exp := "never"
		if r.ExpiresAt != nil {
			exp = r.ExpiresAt.Format("2006-01-02")
		}
		rows = append(rows, []string{r.ID, r.Name, r.Scope, r.Owner, state, "…" + r.Hint, r.CreatedAt.Format("2006-01-02"), exp, ago(r.LastUsed)})
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.out, "No API tokens.")
	} else {
		fmt.Fprintln(c.out, table(rows, []string{"ID", "NAME", "SCOPE", "OWNER", "STATE", "ENDS", "CREATED", "EXPIRES", "LAST USED"}))
	}
	return 0, nil
}

// tokensRevoke revokes a token by id or name; it stops working immediately.
func (c *cli) tokensRevoke(store *tokens.Store, args []string) (int, error) {
	if len(args) != 2 {
		return 2, errUsage
	}
	r, err := store.Revoke(args[1])
	if errors.Is(err, tokens.ErrNotFound) {
		fmt.Fprintf(c.errw, "no active token with id or name %q\n", args[1])
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	c.log("token.revoke", r.Name, map[string]any{"id": r.ID})
	fmt.Fprintf(c.out, "Revoked API token %q (id %s). It stops working immediately.\n", r.Name, r.ID)
	return 0, nil
}
