package fstools

import (
	"context"
	"path/filepath"
	"strings"
)

// Entregáveis de um Loop: `artifacts/` dentro de um loop É o do loop.
//
// Isto começou como instrução no prompt ("salve em loops/<slug>/artifacts/") e
// falhou em produção: num turno de automação o agente gravou em `artifacts/`
// global mesmo com a instrução presente. Não é desobediência gratuita —
// "artifacts/" aparece em outros pontos do prompt (o aviso de teto de
// iterações, por exemplo, manda salvar o resultado parcial ali), e instrução
// perde para instrução.
//
// Quando o comportamento precisa ser garantido, ele vira código. Aqui a
// tradução acontece na resolução do caminho, então vale para toda ferramenta de
// arquivo de uma vez, sem depender de o modelo lembrar.
//
// LIMITE HONESTO: o shell não passa por aqui. `exec` com um redirecionamento
// para `artifacts/x.md` continua escrevendo no global — não há como interceptar
// uma linha de shell arbitrária sem reescrevê-la, o que seria pior. A instrução no prompt
// segue existindo para esse caso.

// loopRootFromContext devolve a raiz do Loop do turno, ou "". Injetado pelo
// pacote agent, que é quem conhece o turno — este pacote não pode importá-lo
// (agent já importa tools).
var loopRootFromContext func(ctx context.Context) string

// SetLoopRootResolver instala o resolvedor. Sem ele, nada é redirecionado: o
// comportamento é exatamente o anterior.
func SetLoopRootResolver(fn func(context.Context) string) {
	loopRootFromContext = fn
}

// artifactsPrefix é o único diretório redirecionado.
//
// Só `artifacts/`, de propósito. `memory/` tem ferramenta própria, com escopo,
// guard-rail e backup — redirecionar a escrita crua daria dois caminhos com
// garantias diferentes para o mesmo arquivo. E `skills/` é escrito pelo
// evolution, que já recebe a raiz do loop.
const artifactsPrefix = "artifacts"

// resolveLoopPath traduz um caminho relativo a `artifacts/` para o loop do
// turno. Devolve o caminho inalterado fora de um loop, ou quando ele não começa
// em `artifacts/`.
//
// Leitura E escrita são redirecionadas. Redirecionar só a escrita deixaria o
// agente incapaz de ler de volta o arquivo que acabou de gravar — uma
// inconsistência pior que a original.
func resolveLoopPath(ctx context.Context, path string) string {
	if loopRootFromContext == nil || ctx == nil {
		return path
	}
	// Caminho absoluto é escolha explícita de quem chamou; não adivinhamos.
	if filepath.IsAbs(path) {
		return path
	}
	root := strings.TrimSpace(loopRootFromContext(ctx))
	if root == "" {
		return path
	}

	clean := filepath.Clean(path)
	if clean != artifactsPrefix && !strings.HasPrefix(clean, artifactsPrefix+string(filepath.Separator)) {
		return path
	}
	// `..` já foi resolvido pelo Clean acima; o validador de caminho a jusante
	// ainda confere que o resultado fica dentro do workspace.
	return filepath.Join(root, clean)
}
