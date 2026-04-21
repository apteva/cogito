// MCP server simulating Deepgram transcription + scoring.
//
// The transcription tool returns a synthetic French sales-call transcript
// sized per DEEPGRAM_TRANSCRIPT_SIZE (characters; default 15000). This is
// deliberately large so the scenario exercises the "big MCP tool result"
// path end-to-end: transcript → rating worker context → docs-agent report
// → final sheet update.
//
// Tools:
//   transcribe(url, model, criteria)
//     Returns {transcript, model, metadata, duration_s, language}.
//     The transcript string length ≈ DEEPGRAM_TRANSCRIPT_SIZE.
//
//   evaluate(transcript, criteria)
//     Returns {score, rationale, highlights[]}. Deterministic hash-based
//     scoring so the scenario's verify step can assert a specific score.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func respond(id int64, result any) {
	data, _ := json.Marshal(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	fmt.Println(string(data))
}

func respondError(id int64, code int, msg string) {
	data, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{code, msg},
	})
	fmt.Println(string(data))
}

func textResult(id int64, text string) {
	respond(id, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}})
}

// syntheticTranscript returns a French sales-call transcript approximately
// `size` characters long. The content is stable for a given url so the
// scenario is reproducible.
func syntheticTranscript(url string, size int) string {
	// Rolling sentence pool — enough variety that the transcript reads as a
	// plausible (if dull) sales call. Keep the pool French so the later
	// "report in French" step doesn't translate accidentally.
	lines := []string{
		"Agent: Bonjour, c'est Marc du service commercial, comment allez-vous aujourd'hui ?",
		"Client: Bonjour, bien merci, je vous écoute.",
		"Agent: Je vous appelle au sujet de votre contrat en cours, j'ai quelques questions pour m'assurer que tout est en ordre.",
		"Client: D'accord, allez-y.",
		"Agent: Pouvez-vous confirmer votre numéro de contrat s'il vous plaît ?",
		"Client: Oui bien sûr, c'est le numéro que vous avez dans vos dossiers je pense.",
		"Agent: Parfait. Je vois que vous avez souscrit il y a environ six mois, est-ce que tout se passe bien ?",
		"Client: Globalement oui, mais j'ai eu un petit souci le mois dernier avec la facturation.",
		"Agent: Je suis désolé d'entendre cela, pouvez-vous me décrire le problème plus précisément ?",
		"Client: J'ai reçu un double prélèvement, mais après appel au support cela a été régularisé.",
		"Agent: Très bien, nous veillons toujours à corriger ce genre d'anomalie rapidement.",
		"Client: Oui, le service client a été réactif, je dois le reconnaître.",
		"Agent: Je me permets de vous présenter quelques nouveautés qui pourraient vous intéresser.",
		"Client: Je vous écoute, mais je n'ai pas beaucoup de temps.",
		"Agent: Je serai bref. Nous avons lancé une option premium qui inclut un accompagnement dédié.",
		"Client: Et quel serait le coût supplémentaire ?",
		"Agent: Vingt euros par mois, avec un engagement de six mois seulement.",
		"Client: Hmm, je vais y réfléchir et vous rappeler la semaine prochaine.",
		"Agent: Bien sûr, je vous envoie un récapitulatif par email. Bonne journée.",
		"Client: Merci, à vous aussi.",
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[Transcript mock url=%s]\n", url))
	for b.Len() < size {
		b.WriteString(lines[b.Len()%len(lines)])
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > size {
		out = out[:size] + "\n[...truncated by mock...]"
	}
	return out
}

// deterministicScore produces a score in [1,10] from the url + criteria so
// the scenario verifier can match against it.
func deterministicScore(url, criteria string) int {
	h := sha1.Sum([]byte(url + "|" + criteria))
	n := binary.BigEndian.Uint32(h[:4])
	return int(n%10) + 1
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "transcribe":
		url := args["url"]
		model := args["model"]
		if url == "" {
			respondError(id, -32602, "url required")
			return
		}
		if model == "" {
			model = "nova-3"
		}
		size := 15000
		if env := os.Getenv("DEEPGRAM_TRANSCRIPT_SIZE"); env != "" {
			if v, err := strconv.Atoi(env); err == nil && v > 0 {
				size = v
			}
		}
		transcript := syntheticTranscript(url, size)
		payload := map[string]any{
			"model":      model,
			"language":   "fr",
			"duration_s": 420,
			"transcript": transcript,
			"metadata": map[string]any{
				"channels":         1,
				"words_estimated":  len(strings.Fields(transcript)),
				"characters":       len(transcript),
			},
		}
		data, _ := json.Marshal(payload)
		textResult(id, string(data))

	case "evaluate":
		transcript := args["transcript"]
		criteria := args["criteria"]
		if transcript == "" || criteria == "" {
			respondError(id, -32602, "transcript and criteria required")
			return
		}
		score := deterministicScore(transcript[:min(64, len(transcript))], criteria)
		payload := map[string]any{
			"score":     score,
			"rationale": fmt.Sprintf("Score %d/10 based on the provided criteria. The agent follows the script, handles objections politely, and proposes an upsell.", score),
			"highlights": []string{
				"Ouverture claire et professionnelle.",
				"Gestion de l'objection facturation correcte.",
				"Proposition d'upsell présentée brièvement.",
			},
		}
		data, _ := json.Marshal(payload)
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toolSchemas() []map[string]any {
	str := func(desc string) map[string]string { return map[string]string{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "transcribe",
			"description": "Transcribe an audio file by URL using Deepgram. Returns {transcript, model, language, duration_s, metadata}. Pass model=nova-3 for best accuracy on French sales calls.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":   str("S3 or https URL to the audio file"),
					"model": str("Deepgram model, e.g. nova-3"),
				},
				"required": []string{"url"},
			},
		},
		{
			"name":        "evaluate",
			"description": "Score a call transcript against a criteria document. Returns {score (1-10), rationale, highlights[]}.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"transcript": str("Full transcript text"),
					"criteria":   str("Evaluation criteria as plain text"),
				},
				"required": []string{"transcript", "criteria"},
			},
		},
	}
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		id := *req.ID
		switch req.Method {
		case "initialize":
			respond(id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "deepgram", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{"tools": toolSchemas()})
		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(id, -32602, "invalid params")
				continue
			}
			handleToolCall(id, params.Name, params.Arguments)
		default:
			respondError(id, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
