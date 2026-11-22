package app

import (
	"testing"

	"github.com/is-koala/koala-api/internal/platform/config"
)

// The relay gets TLS required from the configuration, everywhere but
// local and test (ADR-0032 Karar 4): an environment that is live and
// talks to its relay in plain text would be red here.
func TestSMTPConfigRequiresTLSWhereLive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		env  config.Env
		want bool
	}{
		{config.EnvLocal, false}, {config.EnvTest, false}, {config.EnvStaging, true}, {config.EnvProduction, true},
	} {
		cfg, err := config.Load([]string{
			"KOALA_ENV=" + string(tc.env),
			config.VarSMTPHost + "=smtp.example.com",
			config.VarSMTPPort + "=2525",
			config.VarSMTPUsername + "=koala",
			config.VarSMTPPassword + "=test-smtp-secret",
			config.VarMailFrom + "=no-reply@iskoala.com",
			config.VarWebBaseURL + "=https://iskoala.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := smtpConfig(cfg)
		if got.RequireTLS != tc.want {
			t.Errorf("%s: RequireTLS = %t, want %t", tc.env, got.RequireTLS, tc.want)
		}
		if got.Host != "smtp.example.com" || got.Port != 2525 || got.Username != "koala" ||
			got.Password != "test-smtp-secret" || got.From != "no-reply@iskoala.com" {
			t.Errorf("%s: the relay configuration is not the loaded one", tc.env)
		}
	}
}
