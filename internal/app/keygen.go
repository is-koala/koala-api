package app

import (
	"fmt"
	"io"

	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

const keygenUsage = `usage: koala keygen <kind>

kinds:
  jwt <kid>   a signing key entry "kid:seed" for KOALA_JWT_MARKETPLACE_KEYS
              or KOALA_JWT_STAFF_KEYS (at most 4 entries per ring; the
              first signs)
  pepper      a value for KOALA_LOGIN_PEPPER

The secret is printed to stdout and nowhere else; put it straight into
the deployment's secret store.
`

// runKeygen prints a new secret for the authentication variables
// (ADR-0025 §2). It reads no configuration and touches nothing.
func runKeygen(args []string, sio IO) int {
	switch {
	case len(args) == 2 && args[0] == "jwt":
		entry, err := tokenkeys.NewKeyEntry(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(sio.Stderr, "koala keygen jwt: %v\n", err)
			return ExitUsage
		}
		_, _ = fmt.Fprintln(sio.Stdout, entry)
		return ExitOK
	case len(args) == 1 && args[0] == "pepper":
		_, _ = fmt.Fprintln(sio.Stdout, tokenkeys.NewPepper())
		return ExitOK
	case len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		_, _ = io.WriteString(sio.Stdout, keygenUsage)
		return ExitOK
	default:
		_, _ = io.WriteString(sio.Stderr, keygenUsage)
		return ExitUsage
	}
}
