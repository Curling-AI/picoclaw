package common

import (
	"reflect"
	"testing"
)

// O caso de produção: um servidor MCP declarou senderAddress com lookaround, e
// o provedor recusou a requisição INTEIRA com 400 — deixando o assistente mudo
// em todo turno, porque a lista de tools viaja sempre.
func TestRemoveLookaroundQueDerrubavaOTurno(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"senderAddress": map[string]any{
				"type":        "string",
				"pattern":     `^(?!.*@example\.com).+@.+$`,
				"description": "endereço de quem envia",
			},
		},
	}

	out := StripUnsupportedPatterns(schema)

	props := out["properties"].(map[string]any)
	sender := props["senderAddress"].(map[string]any)
	if _, ok := sender["pattern"]; ok {
		t.Error("pattern com lookaround sobreviveu — a requisição continuaria tomando 400")
	}
	// O resto do campo tem que ficar: perder a descrição custaria qualidade da
	// chamada sem necessidade nenhuma.
	if sender["type"] != "string" || sender["description"] != "endereço de quem envia" {
		t.Errorf("limpeza levou junto o que não devia: %v", sender)
	}
}

// Pattern comum é uma DICA útil ao modelo e não pode ser removido junto.
func TestPatternValidoSobrevive(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"cep": map[string]any{"type": "string", "pattern": `^\d{5}-\d{3}$`},
		},
	}
	out := StripUnsupportedPatterns(schema)
	cep := out["properties"].(map[string]any)["cep"].(map[string]any)
	if cep["pattern"] != `^\d{5}-\d{3}$` {
		t.Errorf("pattern válido foi removido: %v", cep)
	}
}

// Schemas reais aninham em arrays e objetos; a limpeza precisa alcançar tudo.
func TestLimpezaAlcancaAninhados(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"destinatarios": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "pattern": `(?<=@)\w+`},
			},
		},
		"anyOf": []any{
			map[string]any{"pattern": `(?=x)`},
			map[string]any{"pattern": `^ok$`},
		},
	}
	out := StripUnsupportedPatterns(schema)

	items := out["properties"].(map[string]any)["destinatarios"].(map[string]any)["items"].(map[string]any)
	if _, ok := items["pattern"]; ok {
		t.Error("lookaround dentro de array.items sobreviveu")
	}
	any0 := out["anyOf"].([]any)[0].(map[string]any)
	if _, ok := any0["pattern"]; ok {
		t.Error("lookaround dentro de anyOf sobreviveu")
	}
	any1 := out["anyOf"].([]any)[1].(map[string]any)
	if any1["pattern"] != `^ok$` {
		t.Error("pattern válido dentro de anyOf foi removido")
	}
}

// Não pode mutar o schema de entrada: ele é reusado entre turnos, e uma
// limpeza destrutiva apagaria o pattern do registro da tool para sempre.
func TestNaoMutaOSchemaOriginal(t *testing.T) {
	orig := map[string]any{"pattern": `(?!x)`}
	snapshot := map[string]any{"pattern": `(?!x)`}
	StripUnsupportedPatterns(orig)
	if !reflect.DeepEqual(orig, snapshot) {
		t.Errorf("schema de entrada foi mutado: %v", orig)
	}
}
