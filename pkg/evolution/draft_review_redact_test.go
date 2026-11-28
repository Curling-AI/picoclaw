package evolution

import (
	"strings"
	"testing"
)

// A step's argument reaches the task record and the draft prompt: the shell
// forms of a credential and a whole private key must not.
func TestRedactSensitiveTakesShellCredentialsAndWholeKeys(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\nQyNTUxOQAAACDx\n-----END OPENSSH PRIVATE KEY-----"
	cases := map[string]struct{ in, secret, keep string }{
		"curl -u": {
			"curl -u ana:s3nh4Forte https://api.loja.dev/pedidos",
			"s3nh4Forte",
			"curl -u [redacted] https://api.loja.dev/pedidos",
		},
		"curl --user quoted": {"curl --user 'ana:s3nh4Forte' https://api.loja.dev", "s3nh4Forte", "--user [redacted]"},
		"authorization": {
			`curl -H "Authorization: Basic YW5hOnMzbmg0" https://api.loja.dev`,
			"YW5hOnMzbmg0",
			"Authorization: [redacted]",
		},
		"env token": {
			"GITHUB_TOKEN=ghx_abc123def456 gh pr list",
			"ghx_abc123def456",
			"GITHUB_TOKEN=[redacted] gh pr list",
		},
		"env password": {"PGPASSWORD=hunter22 psql -h db -U app", "hunter22", "PGPASSWORD=[redacted] psql"},
		"url userinfo": {
			"psql postgres://app:hunter22@db:5432/loja",
			"hunter22",
			"postgres://[redacted]@db:5432/loja",
		},
		"mysql -p": {"mysqldump -uroot -phunter22 loja > dump.sql", "hunter22", "-p[redacted] loja"},
		"whole private key": {
			"printf '" + key + "' > id_ed25519",
			"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ",
			"printf '[redacted]' > id_ed25519",
		},
		"key cut by the clip": {
			"echo '-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn'",
			"MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn",
			"echo '[redacted]",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := redactSensitive(c.in)
			if strings.Contains(got, c.secret) {
				t.Fatalf("redactSensitive(%q) = %q, still has %q", c.in, got, c.secret)
			}
			if !strings.Contains(got, c.keep) {
				t.Fatalf("redactSensitive(%q) = %q, want it to keep %q", c.in, got, c.keep)
			}
		})
	}
	for _, plain := range []string{
		"python3 - <<'EOF'\nimport csv\nprint(sum(1 for _ in csv.DictReader(open('artifacts/vendas-agosto.csv'))))\nEOF",
		"head -5 exports/pedidos-novembro.csv && wc -l exports/pedidos-novembro.csv",
		"curl -s https://api.loja.dev/status -o status.json",
	} {
		if got := redactSensitive(plain); got != plain {
			t.Errorf("redactSensitive changed a command with no secret:\n%q\n%q", plain, got)
		}
	}
}
