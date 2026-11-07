## Problema

Jobs de cron que não têm nada a reportar mesmo assim mandam mensagem para o chat do usuário. Causa raiz (verificada linha a linha):

1. A nota de sistema de toda run agendada (`pkg/tools/cron.go`) pede "run silently when there is nothing new" — o modelo obedece **narrando** o silêncio ("Silent turn — no text.", "(vazio)"), e a narração vira `finalContent`.
2. `pkg/agent/turn_coord.go`: resposta vazia é substituída por `DefaultResponse` ("The model returned an empty response. This may indicate a provider error or token limit.").
3. `ExecuteJobWithResult` publica qualquer `result.Content != ""` no canal do job.
4. Turns de cron também publicam tool feedback e interim pico/grpc no chat.

## Solução: flag `silent` por job

`CronPayload.Silent` → `turnresult.Request.Silent` → `processMessageResult(..., silent)` → `opts.Silent`.

Com `silent: true`, o turno não entrega nada ao chat por conta própria — nem resposta final (mesmo narrada), nem feedback de ferramenta, nem saída de command que deu certo (**falha continua indo** — job quebrado é novidade). O que for para o usuário sai pela ferramenta `message`.

Cobre todos os caminhos de entrega sem passar pela message tool:
- `turn_coord`: resposta vazia não vira DefaultResponse nem fallback
- `pipeline_llm`: interim pico/grpc desligado; raciocínio não vira resposta; retry de resposta vazia só antes da primeira ferramenta
- `pipeline_finalize`: fim vazio não persiste como reply
- cron tool: publish pulado; nota de sistema instrui resposta vazia em vez de "run silently"
- resposta vazia em turno silent reporta `Completed`, não `Failed`

## Garantias

- Jobs **não-silent**: comportamento idêntico ao anterior (prompt byte-idêntico, `silentRunNote` retorna "") — travado por teste.
- Mensagens de usuário, heartbeat, subturn, steering, /btw: zero mudança (`Silent` só é setado no caminho cron).
- `add`/`update` do cron tool aceitam `silent`; `update` sem o campo preserva o valor atual.

## Testes

7 testes novos (`pkg/tools/cron_silent_test.go`, `pkg/agent/silent_turn_test.go`):
- silent publica nada (mesmo com resposta narrada)
- message tool continua entregando em job silent
- pico/grpc não recebe interim em turno silent
- command silent publica só falhas
- add/update do flag
- job não-silent: prompt e publish inalterados

`go test ./pkg/tools/... ./pkg/cron/...` verde; `pkg/agent` verde nos testes de silent (2 falhas pré-existentes no main: TestGlobalSkillFileContentChange, TestSkillDiscovery_DefersCatalogToHint — reproduzem sem o patch).
