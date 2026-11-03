package redact

import (
	"strings"
	"testing"
)

func TestText_MasksCredentials(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		secret string
	}{
		{
			name:   "authorization header with bearer",
			in:     `curl -H "Authorization: Bearer abcdefghijklmnopqrstuvwx" https://api.example.com`,
			want:   `curl -H "Authorization: Bearer [REDACTED]" https://api.example.com`,
			secret: "abcdefghijklmnopqrstuvwx",
		},
		{
			name:   "authorization header with basic",
			in:     `curl -H 'authorization: Basic dXNlcjpodW50ZXIy' https://x`,
			want:   `curl -H 'authorization: Basic [REDACTED]' https://x`,
			secret: "dXNlcjpodW50ZXIy",
		},
		{
			name:   "authorization as a JSON field",
			in:     `{"headers":{"Authorization":"Bearer abcdefghijklmnop1234"}}`,
			secret: "abcdefghijklmnop1234",
		},
		{
			name:   "authorization as an http.Header list",
			in:     `{"Authorization":["Basic dXNlcjpodW50ZXIy"]}`,
			secret: "dXNlcjpodW50ZXIy",
		},
		{
			name:   "bare bearer token",
			in:     `token is Bearer abcdefghijklmnop1234`,
			want:   `token is Bearer [REDACTED]`,
			secret: "abcdefghijklmnop1234",
		},
		{
			name:   "cookie header",
			in:     `curl -H 'Cookie: sessionid=hunter2; csrftoken=abc' https://x`,
			want:   `curl -H 'Cookie: [REDACTED]' https://x`,
			secret: "hunter2",
		},
		{
			name:   "curl -b",
			in:     `curl -b 'sessionid=hunter2' https://x`,
			want:   `curl -b [REDACTED] https://x`,
			secret: "hunter2",
		},
		{
			name:   "password in a URL",
			in:     `psql postgres://admin:hunter2@db.example.com:5432/app`,
			want:   `psql postgres://admin:[REDACTED]@db.example.com:5432/app`,
			secret: "hunter2",
		},
		{
			name:   "password in a URL with no user",
			in:     `redis-cli -u redis://:hunter2@cache:6379`,
			want:   `redis-cli -u redis://:[REDACTED]@cache:6379`,
			secret: "hunter2",
		},
		{
			name:   "token in a query string",
			in:     `GET https://x/artifacts/1?token=hunter2&download=1`,
			want:   `GET https://x/artifacts/1?token=[REDACTED]&download=1`,
			secret: "hunter2",
		},
		{
			name:   "curl -u",
			in:     `curl -u admin:hunter2 https://x`,
			want:   `curl -u admin:[REDACTED] https://x`,
			secret: "hunter2",
		},
		{
			name:   "curl -u glued",
			in:     `curl -uadmin:hunter2 https://x`,
			want:   `curl -uadmin:[REDACTED] https://x`,
			secret: "hunter2",
		},
		{
			name:   "curl -u with a quoted password",
			in:     `curl -u admin:'hunter 2' https://x`,
			want:   `curl -u admin:[REDACTED] https://x`,
			secret: "hunter",
		},
		{
			name:   "curl -u with a quoted word",
			in:     `curl -s -u 'admin:hunter 2' https://x`,
			want:   `curl -s -u 'admin:[REDACTED]' https://x`,
			secret: "hunter",
		},
		{
			name:   "curl --user=",
			in:     `curl --user=admin:hunter2 https://x`,
			want:   `curl --user=admin:[REDACTED] https://x`,
			secret: "hunter2",
		},
		{
			name:   "mysql -p glued",
			in:     `mysql -h db -u root -phunter2 app`,
			want:   `mysql -h db -u root -p[REDACTED] app`,
			secret: "hunter2",
		},
		{
			name:   "sshpass",
			in:     `sshpass -p hunter2 ssh root@host`,
			want:   `sshpass -p [REDACTED] ssh root@host`,
			secret: "hunter2",
		},
		{
			name:   "password flag",
			in:     `psql --password=hunter2 && mongo --pass 'hunter 3'`,
			want:   `psql --password=[REDACTED] && mongo --pass [REDACTED]`,
			secret: "hunter",
		},
		{
			name:   "secret environment variables",
			in:     `DB_PASS=hunter2 MYSQL_PWD=hunter3 STRIPE_KEY=hunter4 ./run`,
			want:   `DB_PASS=[REDACTED] MYSQL_PWD=[REDACTED] STRIPE_KEY=[REDACTED] ./run`,
			secret: "hunter",
		},
		{
			name:   "JWT",
			in:     `session eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.c2lnbmF0dXJlMTIz end`,
			want:   `session [REDACTED] end`,
			secret: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name:   "PEM private key in a JSON string",
			in:     `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\nkqhkiG9w0B\n-----END PRIVATE KEY-----\n","client_email":"x@y"}`,
			secret: "MIIEvQIBADANBg",
		},
		{
			name:   "PEM private key cut short by a preview",
			in:     `write_file({"content":"-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0B...`,
			secret: "MIIEvQIBADANBg",
		},
		{
			name:   "Hulk key keeps its public part",
			in:     `key hulk_invoke.key_ABCDEFGHIJKLMNOPQRSTUVWX.abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG`,
			want:   `key hulk_invoke.key_ABCDEFGHIJKLMNOPQRSTUVWX.[REDACTED]`,
			secret: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
		},
		{
			name:   "Anthropic key",
			in:     `export K=sk-ant-api03-abcdefghijklmnopqrstuvwxyz`,
			secret: "abcdefghijklmnopqrstuvwxyz",
		},
		{
			name:   "GitHub token",
			in:     `git clone https://ghp_abcdefghijklmnopqrstuvwxyz0123@github.com/x/y`,
			secret: "ghp_abcdefghijklmnopqrstuvwxyz0123",
		},
		{
			name:   "AWS access key",
			in:     `aws configure set aws_access_key_id AKIAABCDEFGHIJKLMNOP`,
			secret: "AKIAABCDEFGHIJKLMNOP",
		},
		{
			name:   "Telegram bot token",
			in:     `https://api.telegram.org/bot123456789:AAHabcdefghijklmnopqrstuvwxyz012345/getMe`,
			secret: "AAHabcdefghijklmnopqrstuvwxyz012345",
		},
		{
			name:   "password in a form body",
			in:     `POST /login user=ana&password=hunter2&next=/home`,
			want:   `POST /login user=ana&password=[REDACTED]&next=/home`,
			secret: "hunter2",
		},
		{
			name:   "quoted JSON value with spaces",
			in:     `{"email":"ana@example.com","password":"correct horse battery"}`,
			want:   `{"email":"ana@example.com","password":"[REDACTED]"}`,
			secret: "horse",
		},
		{
			name:   "quoted JSON value with an escaped quote",
			in:     `{"password":"hun\"ter2","next":1}`,
			want:   `{"password":"[REDACTED]","next":1}`,
			secret: "ter2",
		},
		{
			name:   "JSON escaped inside a string",
			in:     `{"command":"curl -d '{\"senha\":\"hunter2\"}' https://x"}`,
			want:   `{"command":"curl -d '{\"senha\":\"[REDACTED]\"}' https://x"}`,
			secret: "hunter2",
		},
		{
			name:   "fat arrow assignment",
			in:     `{ password => 'hunter2' }`,
			want:   `{ password => '[REDACTED]' }`,
			secret: "hunter2",
		},
		{
			name:   "environment assignment",
			in:     `GITHUB_TOKEN=abc123xyz make deploy`,
			want:   `GITHUB_TOKEN=[REDACTED] make deploy`,
			secret: "abc123xyz",
		},
		{
			name:   "keys under short shell names",
			in:     `DDK=0123456789abcdef0123456789abcdef DDA=ddapp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh curl https://api.datadoghq.com/api/v1/validate`,
			want:   `DDK=[REDACTED] DDA=[REDACTED] curl https://api.datadoghq.com/api/v1/validate`,
			secret: "0123456789abcdef",
		},
		{
			name:   "exported keys under names without a secret word",
			in:     `export DD_APP=ddapp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh DD_API=0123456789abcdef0123456789abcdef; python3 q.py`,
			want:   `export DD_APP=[REDACTED] DD_API=[REDACTED]; python3 q.py`,
			secret: "0123456789abcdef",
		},
		{
			name:   "keys under single-letter names",
			in:     `cd work && A=ddapp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh K=Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2 ./q.sh`,
			want:   `cd work && A=[REDACTED] K=[REDACTED] ./q.sh`,
			secret: "Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2",
		},
		{
			name:   "quoted keys under short shell names",
			in:     `DDK="0123456789abcdef0123456789abcdef" K='Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2' ./q.sh`,
			want:   `DDK="[REDACTED]" K='[REDACTED]' ./q.sh`,
			secret: "0123456789abcdef",
		},
		{
			name:   "secret-named quoted value is masked to its closing quote",
			in:     `PASSWORD="Xk9mP2qR7sT4vW8yZ1aB3c tail-of-secret" ./run`,
			want:   `PASSWORD="[REDACTED]" ./run`,
			secret: "tail-of-secret",
		},
		{
			name:   "token under a short name after an escaped line break",
			in:     `{"command":"cat <<EOF > .env\nDDK=0123456789abcdef0123456789abcdef\nEOF"}`,
			want:   `{"command":"cat <<EOF > .env\nDDK=[REDACTED]\nEOF"}`,
			secret: "0123456789abcdef",
		},
		{
			name:   "token followed by a redirection or punctuation",
			in:     `echo DDK=0123456789abcdef0123456789abcdef>>.env && [K=Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2] T=Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2.`,
			want:   `echo DDK=[REDACTED]>>.env && [K=[REDACTED]] T=[REDACTED].`,
			secret: "Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2",
		},
		{
			name:   "base64 token with a slash",
			in:     `K=ab12/cd34efgh5678ijkl9012mnop== ./x`,
			want:   `K=[REDACTED] ./x`,
			secret: "cd34efgh5678",
		},
		{
			name:   "token under a short name inside an escaped JSON command",
			in:     `{"command":"bash -c \"DDK=0123456789abcdef0123456789abcdef curl -s https://x\""}`,
			want:   `{"command":"bash -c \"DDK=[REDACTED] curl -s https://x\""}`,
			secret: "0123456789abcdef",
		},
		{
			name:   "Datadog application key anywhere",
			in:     `the key is ddapp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh, use it`,
			want:   `the key is [REDACTED], use it`,
			secret: "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		},
		{
			name:   "Datadog key headers",
			in:     `curl -H "DD-API-KEY: 0123456789abcdef0123456789abcdef" -H "DD-APPLICATION-KEY: ddapp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh" https://x`,
			want:   `curl -H "DD-API-KEY: [REDACTED]" -H "DD-APPLICATION-KEY: [REDACTED]" https://x`,
			secret: "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		},
		{
			name:   "application key in the older hex format",
			in:     `curl -H 'DD-APPLICATION-KEY: 0123456789abcdef0123456789abcdef01234567' https://x`,
			want:   `curl -H 'DD-APPLICATION-KEY: [REDACTED]' https://x`,
			secret: "0123456789abcdef",
		},
		{
			name:   "application key as a JSON field",
			in:     `{"application_key":"0123456789abcdef0123456789abcdef01234567","app_key":"hunter2"}`,
			want:   `{"application_key":"[REDACTED]","app_key":"[REDACTED]"}`,
			secret: "0123456789abcdef",
		},
		{
			name:   "labeled password in prose",
			in:     `Senha: hunter2`,
			want:   `Senha: [REDACTED]`,
			secret: "hunter2",
		},
		{
			name:   "single-quoted YAML value",
			in:     `api_key: 'abc def'`,
			want:   `api_key: '[REDACTED]'`,
			secret: "abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.in)
			if tt.want != "" && got != tt.want {
				t.Fatalf("Text(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, tt.secret) {
				t.Fatalf("Text(%q) = %q still contains %q", tt.in, got, tt.secret)
			}
		})
	}
}

func TestText_LeavesNonSecretsAlone(t *testing.T) {
	tests := []string{
		`curl -H "Authorization: Bearer $TOKEN" https://x`,
		`TOKEN=$(cat work/token.txt); curl -H "Authorization: Bearer ${TOKEN}" https://x`,
		`{"model":"x","max_tokens":100,"token_count":512}`,
		`secretName: datadog-secret`,
		`tokenizer: cl100k`,
		`the token expired yesterday`,
		`{"tool":"exec","iteration":3}`,
		`see https://example.com/path?page=2`,
		`docker run -u 1000:1000 alpine && ps -u root:root`,
		`mkdir -p work/out && ssh -p 22 host`,
		`cd $PWD && PWD=/home/ethos ls`,
		`task-runner sk-short`,
		`password: <your-password>`,
		`{"password":""}`,
		`password_changed: true`,
		`10.0.0.1 - - [26/Oct/2026] "GET /api/v1/users?limit=5 HTTP/1.1" 200`,
		// Shell assignments whose value is not a token.
		`NODE_ENV=production CI=1 npm run build`,
		`RUN_ID=6631cfd5-7665-4b92-96c1-20f96fce66ae ./run`,
		`PYTHONPATH=/home/ethos/workspace/lib/python3 python3 x.py`,
		`WORKSPACE=/home/ethos/workspace/project2026 ./build`,
		`NODE_OPTIONS=--max-old-space-size=4096 node x.js`,
		`BUILD=20261103150000123456 STAGE=productionreleasecandidate make`,
		`HOST=ip-10-0-1-23-abcdefghijk.ec2.internal curl https://x`,
		`IMAGE=registry-0123456789abcdef:5000/app docker pull "$IMAGE"`,
		`FROM=release-bot-2026-abcdef@example.com ./notify`,
		// Lowercase names are log fields and URL parameters, not shell variables.
		`INF tool registry.go:409 > Tool execution started tool=mcp_composio_gmail_fetch_emails_v2`,
		`https://cdn.example.com/x.jpg?_nc_ht=scontent-gru2-1.xx&oh=00_AYBabcdef1234567890xyz`,
	}
	for _, in := range tests {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}
