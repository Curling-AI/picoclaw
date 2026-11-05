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

func TestText_MasksUploadNames(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "relative path",
			in:   `uploads/ab12cd34-Contrato_Acme_2026.pdf`,
			want: `uploads/ab12cd34-[REDACTED].pdf`,
		},
		{
			name: "absolute path",
			in:   `/home/ethos/.assistant/workspace/uploads/ab12cd34-Contrato_Acme.docx`,
			want: `/home/ethos/.assistant/workspace/uploads/ab12cd34-[REDACTED].docx`,
		},
		{
			name: "tool call preview",
			in:   `Tool call: read_file({"path":"uploads/ab12cd34-Contrato_Acme.pdf"})`,
			want: `Tool call: read_file({"path":"uploads/ab12cd34-[REDACTED].pdf"})`,
		},
		{
			name: "shell command",
			in:   `pdftotext uploads/ab12cd34-Contrato_Acme.pdf -`,
			want: `pdftotext uploads/ab12cd34-[REDACTED].pdf -`,
		},
		{
			name: "two uploads in one line",
			in:   `cat uploads/ab12cd34-a_b.txt uploads/00ff00ff-c_d.csv`,
			want: `cat uploads/ab12cd34-[REDACTED].txt uploads/00ff00ff-[REDACTED].csv`,
		},
		{
			name: "no extension",
			in:   `uploads/ab12cd34-Contrato_Acme`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "text after a dot is not an extension",
			in:   `uploads/ab12cd34-Contrato.Acme_Sigiloso`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "only the last extension stays",
			in:   `uploads/ab12cd34-Contrato.Acme.tar.gz`,
			want: `uploads/ab12cd34-[REDACTED].gz`,
		},
		{
			name: "masking twice changes nothing",
			in:   `uploads/ab12cd34-[REDACTED].pdf`,
			want: `uploads/ab12cd34-[REDACTED].pdf`,
		},
		{
			name: "a command run inside uploads/",
			in:   `cd uploads && pdftotext ab12cd34-Contrato_Acme.pdf -`,
			want: `cd uploads && pdftotext ab12cd34-[REDACTED].pdf -`,
		},
		{
			name: "an exec whose cwd is uploads/",
			in:   `{"command":"cat ab12cd34-Contrato_Acme.txt","cwd":"uploads"}`,
			want: `{"command":"cat ab12cd34-[REDACTED].txt","cwd":"uploads"}`,
		},
		{
			name: "a suffix that is part of the name",
			in:   `uploads/ab12cd34-Laudo.Silva`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "a number after the dot",
			in:   `uploads/ab12cd34-NF.12345`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "an extension in capitals",
			in:   `uploads/ab12cd34-Foto_Silva.JPG`,
			want: `uploads/ab12cd34-[REDACTED].JPG`,
		},
		{
			name: "a name of digits, like a CPF",
			in:   `uploads/ab12cd34-12345678900`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "a name like a date",
			in:   `uploads/ab12cd34-2026-11-03`,
			want: `uploads/ab12cd34-[REDACTED]`,
		},
		{
			name: "after an escaped newline",
			in:   `ls:\nab12cd34-Joao_Silva.pdf`,
			want: `ls:\nab12cd34-[REDACTED].pdf`,
		},
		{
			name: "every name of an ls serialized to JSON",
			in:   `{"output":"ab12cd34-A.pdf\nab12cd34-B.pdf"}`,
			want: `{"output":"ab12cd34-[REDACTED].pdf\nab12cd34-[REDACTED].pdf"}`,
		},
		{
			name: "after an escaped tab",
			in:   `a\tab12cd34-X.pdf`,
			want: `a\tab12cd34-[REDACTED].pdf`,
		},
		{
			name: "after a percent-encoded slash",
			in:   `uploads%2Fab12cd34-X.pdf`,
			want: `uploads%2Fab12cd34-[REDACTED].pdf`,
		},
		{
			name: "after a JSON unicode escape",
			in:   `\u003cab12cd34-X.pdf\u003e`,
			want: `\u003cab12cd34-[REDACTED].pdf\u003e`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.in); got != tt.want {
				t.Fatalf("Text(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestText_LeavesNonSecretsAlone(t *testing.T) {
	tests := []string{
		`ls uploads/`,
		`uploads/notes.txt`,
		`uploads/ab12cd34`,
		// A UUID names a run or a session; its tail is hex and dashes.
		`run 3f2c1a9e-1b2c-4d5e-8f90-123456789abc started`,
		`/ws/sessions/3f2c1a9e-1b2c-4d5e-8f90-123456789abc.jsonl`,
		`chat 3f2c1a9e-1b2c-4d5e-8f90-123456789abc.`,
		`media/3f2c1a9e-1b2c-4d5e-8f90-123456789abc.ogg`,
		`job 3f2c1a9e-1b2c-4d5e-8f90-123456789abc_retry`,
		`ls:\n3f2c1a9e-1b2c-4d5e-8f90-123456789abc.jsonl`,
		`media%2F3f2c1a9e-1b2c-4d5e-8f90-123456789abc.ogg`,
		`{"session_id":"3f2c1a9e-1b2c-4d5e-8f90-123456789abc"}`,
		// A hex part inside a longer name does not start the token.
		`pod maestro-gateway-ab12cd34-x9k2p ready`,
		`agent:cron-job-17800000-0000-4000-8000-000000000031`,
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
	}
	for _, in := range tests {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}

func BenchmarkText(b *testing.B) {
	for _, line := range []string{
		`Tool call: exec({"command":"ls -la work/2026-11-03","session_id":"pc-12"})`,
		`run 3f2c1a9e-1b2c-4d5e-8f90-123456789abc finished in 1.2s`,
		`Tool call: read_file({"path":"uploads/ab12cd34-Contrato_Acme.pdf"})`,
	} {
		b.Run(line[:20], func(b *testing.B) {
			for b.Loop() {
				Text(line)
			}
		})
	}
}
