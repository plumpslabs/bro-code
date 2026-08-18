# Blueprint: Minimalist Autonomous Agentic Coding CLI (Go)

**Status:** Draft v1 — living document, update sebagai keputusan berubah.
**Prinsip inti:** 10% effort di UI/design, 90% di core loop + context engineering. Kalau loop-nya lemah, UI secantik apapun nggak nolong.

---

## 0. Kenapa Ini Beda dari Sekadar "Bikin CLI Chat dengan Tool Calling"

Riset arsitektur 13 open-source coding agent scaffold (Inside the Scaffold, arXiv 2604.03515) nemuin insight penting: **agent yang keliatan "lebih pintar" itu jarang soal model beda — itu soal scaffold**. Tool count berkisar 0–37, compaction strategy ada 7 varian berbeda, dan 11 dari 13 agent nge-compose *lebih dari satu* loop primitive (bukan cuma ReAct polos). Jadi target lo bukan "bikin 1 loop yang jalan", tapi bikin **beberapa primitive yang bisa dikombinasi** tergantung jenis task.

Lima loop primitive yang dipetakan riset ini:
1. **ReAct** — reason → act → observe → repeat. Default buat eksplorasi & task terbuka.
2. **Plan-Execute** — bikin rencana eksplisit dulu, baru eksekusi step by step, re-plan kalau meleset.
3. **Generate-Test-Repair** — tulis kode → jalanin test → kalau gagal, perbaiki (loop terbatas).
4. **Multi-attempt retry** — coba beberapa strategi berbeda, pilih yang paling meyakinkan.
5. **Tree search** — eksplor beberapa cabang solusi paralel (biasanya overkill buat CLI ringan, skip dulu di v1).

Buat brocode lo: **ReAct sebagai default loop**, dengan **Plan-Execute sebagai mode eksplisit** yang dipicu pas task-nya kompleks (multi-file, butuh urutan kerja jelas), dan **Generate-Test-Repair** sebagai sub-loop yang nempel di ReAct pas ada test runner terdeteksi di project.

---

## 1. Arsitektur Layer (High Level)

```
┌─────────────────────────────────────────────────────────┐
│  UI Layer (Bubble Tea v2)  — render only, no logic       │
├─────────────────────────────────────────────────────────┤
│  Session Orchestrator      — the agent loop itself       │
│    ├─ Planner (opsional, plan-execute mode)               │
│    ├─ ReAct core loop                                     │
│    └─ Verifier (self-check sebelum turn selesai)          │
├─────────────────────────────────────────────────────────┤
│  Context Manager                                          │
│    ├─ Message store (event log, append-only)              │
│    ├─ Compactor (trigger-based, structured summary)        │
│    └─ Memory (chromem-go — semantic recall lintas sesi)   │
├─────────────────────────────────────────────────────────┤
│  Tool Layer                                                │
│    ├─ Native tool registry (read/write/edit/bash/grep)    │
│    ├─ MCP client (stdio/http/sse)                          │
│    └─ Skill loader (SKILL.md + fuzzy matcher — udah ada)   │
├─────────────────────────────────────────────────────────┤
│  Provider Layer                                             │
│    └─ Adapter per-provider, native function-calling only   │
│       (jangan reinvent text-block tool call format)         │
├─────────────────────────────────────────────────────────┤
│  Persistence (modernc.org/sqlite)                           │
│    └─ sessions, messages, tool_calls, memory_index          │
└─────────────────────────────────────────────────────────┘
```

Kunci arsitektural: **setiap layer harus bisa diuji terpisah tanpa nyalain LLM beneran** (mock provider). Ini bukan nice-to-have — ini yang bikin lo bisa debug "kenapa agent gue bego" tanpa nebak-nebak kayak kasus transkrip kemarin.

---

## 2. Agent Loop — Jantungnya

### 2.1 State Machine

Loop bukan `for { call LLM; run tool; }` doang. Riset "Stop Hand-Holding Your Coding Agent" (arXiv 2607.00038) nekenin: loop yang bagus punya **trigger, goal, verification step, stopping rule, dan memory** sebagai elemen eksplisit — bukan ke-embed diam-diam di kode.

```go
type LoopState int

const (
    StateThinking LoopState = iota // model reasoning, belum act
    StateActing                     // tool call in flight
    StateObserving                  // hasil tool masuk, siap diproses
    StateVerifying                  // cek apakah goal tercapai
    StateDone                       // terminal: sukses
    StateBlocked                    // terminal: butuh input user
    StateFailed                     // terminal: give up dengan alasan jelas
)
```

**Terminal state harus eksplisit dan punya nama.** Riset loop engineering nemuin 74% loop yang matang itu nge-name terminal state-nya secara eksplisit. Jangan biarin loop lo "berhenti" cuma karena kehabisan tool call tanpa bilang kenapa — itu persis bug yang lo alamin di transkrip kemarin (model stop, nanya balik, padahal belum eksplor cukup).

### 2.2 Thinking-Before-Answer

Ini requirement utama lo. Implementasinya bukan cuma "suruh model mikir" di prompt — itu placebo kalau nggak dipaksa struktural. Dua opsi:

- **Kalau provider support native extended thinking / reasoning tokens** (Claude, beberapa model open-weight kayak laguna-s-2.1 yang punya `enable_thinking`): pakai itu, preserve reasoning block antar tool call (jangan di-strip pas re-inject ke context — riset nunjukin model "lupa" cara reasoning kalau thinking block sebelumnya dibuang).
- **Kalau provider nggak support native reasoning**: paksa via *structured output* — model wajib isi field `reasoning` sebelum `action` di setiap turn (JSON schema atau tool call wrapper), dan **harness lo yang enforce**, jangan cuma minta baik-baik di system prompt.

```go
type AgentTurn struct {
    Reasoning string      `json:"reasoning"`       // wajib diisi, non-empty
    Action    ToolCall     `json:"action,omitempty"`
    Answer    string       `json:"answer,omitempty"` // kalau ini diisi, action harus kosong
}
```

Validasi di harness: kalau `Reasoning` kosong, **reject dan minta ulang** (dengan pesan error yang jelas ke model, bukan diem-diem lanjut). Ini yang bedain "agent yang mikir" vs "agent yang cuma nurut command tanpa pertimbangan" — persis pattern yang bikin transkrip kemarin jawabannya generik ("biasanya", "kemungkinan").

### 2.3 Loop Continuation Rule (paling kritis buat kasus lo)

Dari diagnosa transkrip lo kemarin: masalah utama itu **agent berhenti terlalu cepat dan nanya balik user padahal masih bisa eksplor sendiri**. Fix-nya di level system prompt DAN kode:

```
System prompt rule (contoh, bukan verbatim):
"Setelah tool call selesai dan hasil observation masuk, JANGAN langsung
tanya user kecuali informasi yang lo butuh benar-benar cuma user yang tau
(preferensi, keputusan bisnis, ambiguitas requirement). Kalau ada
ketidakjelasan teknis (file mana, fungsi mana, format apa), CARI SENDIRI
lewat tool. Lanjut loop sampai: (a) goal tercapai, (b) hit max_iterations,
atau (c) beneran blocked oleh keputusan yang cuma user yang bisa jawab."
```

Kode-nya: loop TIDAK exit setelah 1 tool call. Loop lanjut ke `StateThinking` lagi otomatis selama state bukan `Done`/`Blocked`/`Failed`, sampai `max_iterations` (config-able, default misal 25) atau model eksplisit ngirim `Answer` tanpa `Action`.

### 2.4 Verification Ladder

Sebelum masuk `StateDone`, lewatin verification. Level makin tinggi makin mahal tapi makin reliable:

| Level | Cek | Kapan pakai |
|---|---|---|
| 0 | Tidak ada — langsung percaya output model | Jangan, ini anti-pattern |
| 1 | Syntax/format check otomatis (linter, `go vet`, parser) | Semua code edit |
| 2 | Model self-review hasil sendiri (re-read file yang baru diedit) | Semua code edit |
| 3 | Jalanin test suite yang relevan | Kalau project punya test & task-nya nyentuh logic |
| 4 | Diff review terhadap intent awal (bandingin ke plan/requirement) | Task kompleks / multi-file |
| 5 | Human-in-the-loop confirm sebelum commit/push | Aksi destruktif (`rm`, force push, migration) |

Minimal buat v1: **Level 1 & 2 wajib, otomatis, tanpa exception**. Level 3 kalau kedetect ada test command. Level 5 hardcoded untuk command destruktif (whitelist/blacklist).

---

## 3. Context & Memory Management

### 3.1 Compaction Strategy — Pilihan yang Direkomendasikan

Riset compaction 2026 (Zylos Research, x-cmd breakdown Claude Code/Codex/OpenCode) motret spektrum ini:

- **Reactive** (Claude Code): compact pas hit ~98% context window. Simpel, tapi telat — context udah "kotor" duluan sebelum di-compact.
- **Periodic** (sebagian agent research): compact tiap N turn tetap. Predictable tapi bisa motong di tengah subgoal aktif.
- **Prevention** (AutoCodeRover, Moatless Tools): batasin growth dari awal — cap jumlah search round, cuma tampilin sebagian hasil tool per query. **Ini paling murah dan paling cocok buat filosofi "minimalist, efisien" lo.**
- **Dynamic context discovery** (Cursor, 2026): tool output GEDE nggak langsung di-stuff ke context — ditulis ke file/temp, model akses on-demand pakai `tail`/`grep`/`rg`. Cursor lapor total token turun **46.9%** di run yang pakai MCP tools. Ini teknik yang paling relevan buat "codebase besar kecil gede" yang lo sebut.

**Rekomendasi buat brocode:**

1. **Prevention by default**: tool output (`grep`, `find`, `ls`) dibatasi jumlah baris/hasil per call (misal top 50 match, kasih tau kalau ada lebih). Jangan stuff seluruh isi file besar — kasih outline dulu (fungsi/class list), model minta baca section spesifik kalau perlu.
2. **Dynamic discovery buat file besar**: kalau file/hasil > threshold token (misal 4k token), tulis ke `.brocode/tmp/`, kasih model path-nya + ringkasan struktur, biar dia `grep`/baca partial sendiri via tool, bukan di-dump full ke context.
3. **Reactive compaction sebagai safety net**: trigger di ~85% context window (jangan tunggu 98% kayak Claude Code — lo nggak punya prompt caching se-canggih itu di awal, jadi kasih buffer lebih).
4. **Structured summary, bukan freeform**: pas compact, paksa format section tetap (mirip pendekatan OpenCode — "5-heading LLM summary"):
   ```
   ## Goal
   ## Files touched (path + kenapa relevan)
   ## Decisions made (dan alasannya)
   ## Open questions / pending work
   ## Last known state (test pass/fail, error terakhir)
   ```
   Ini jauh lebih reliable buat resume daripada ringkasan naratif bebas — dan gampang di-parse ulang programmatically.
5. **Preserve reasoning block terakhir** kalau lagi di tengah multi-step reasoning aktif — jangan di-compact kalau lagi di tengah subgoal (ini exact failure mode yang disebut riset: "periodic compaction... often summarizing in the middle of an active subgoal").

### 3.2 Event Log, Bukan Flat Message List

Simpan history sebagai **append-only event log** di sqlite (modernc.org/sqlite, sesuai stack lo), bukan cuma array `[]Message` di memory. Kenapa:
- Bisa replay/debug session lama (baca ulang persis apa yang dikirim ke API — ini yang lo butuh buat diagnosa masalah kayak transkrip kemarin).
- Compaction jadi "insert compaction event yang mereferensikan range event lama", event lama nggak dihapus (opsional soft-hide, kayak pendekatan OpenCode — "non-destructive timestamp-based message hiding").
- Gampang bikin `/replay` atau `/debug-context` command buat inspect state persis.

Schema kasar:
```sql
CREATE TABLE sessions (id, created_at, project_path, status);
CREATE TABLE events (id, session_id, seq, type, payload_json, tokens, created_at, hidden_at);
-- type: 'user_msg' | 'reasoning' | 'tool_call' | 'tool_result' | 'compaction_summary' | 'assistant_msg'
CREATE TABLE memory_index (id, session_id, embedding, content, source_event_id); -- via chromem-go
```

### 3.3 Memory Lintas Sesi (chromem-go)

Ini beda dari context window management — ini long-term memory. Dipakai buat:
- "Project facts" yang persisten (arsitektur project, konvensi kode, keputusan yang pernah dibuat) — di-inject selektif di awal sesi baru, bukan semua sekaligus.
- Skill/pattern matching (udah ada prototype fuzzy matcher lo — nyambungin ke sini).

**Jangan** jadiin ini pengganti compaction. Compaction itu soal context window sesi berjalan; memory itu soal knowledge yang bertahan antar sesi. Beda mekanisme, beda tujuan, jangan dicampur.

---

## 4. Tool Layer

### 4.1 Native Function Calling Only

Dari diskusi kita sebelumnya: **jangan bikin format tool-call custom** (text block yang di-parse regex). Model modern (termasuk model open-weight/free kayak Laguna, yang lo pakai) dilatih pakai native JSON tool_calls. Harness lo harus manggil API dengan `tools` schema resmi provider, bukan nyuruh model nulis `\`\`\`bash` block yang lo parse manual.

### 4.2 Tool Set Minimal (v1) — Jangan Kebanyakan

Riset taxonomy nemuin tool count agent open-source berkisar **0–37**, tapi yang paling efektif nggak selalu yang paling banyak tool-nya. Buat "minimalist tapi andal", mulai dari set kecil:

| Tool | Fungsi |
|---|---|
| `read_file` | baca file/section, dengan line range opsional |
| `list_dir` | list isi direktori (bounded depth) |
| `grep` | search pattern, hasil dibatasi + bisa "show more" |
| `edit_file` | edit pakai diff (Myers diff — sesuai stack lo), bukan overwrite full file |
| `write_file` | buat file baru |
| `bash` | eksekusi command, dengan whitelist/confirm buat command destruktif |
| `glob` | cari file by pattern |

Tambahan opsional pas udah stabil: `task` (spawn sub-agent buat sub-task terisolasi, biar context utama nggak numpuk), `mcp_*` (dynamic, dari server MCP yang connect).

### 4.3 Tool Result Formatting

Ini sering diremehin tapi riset nunjukin ini **"primary token budget killer"**. Aturan:
- Selalu kasih **structure**, bukan raw dump: buat `grep`, kasih `file:line: match` bukan seluruh block context sekitarnya kecuali diminta.
- Buat `read_file`, kalau file > threshold, kasih outline (daftar function/class + line number) dulu, bukan seluruh isi.
- Selalu kasih **truncation notice eksplisit**: `"[showing 50/230 matches, refine query or ask for more]"` — biar model tau ada lebih, bukan diem-diem kepotong.

### 4.4 MCP & SKILL.md Compatibility

Udah sesuai arah lo (AGENTS.md, MCP, SKILL.md convention). Tambahan: pastikan **skill loading lazy & fuzzy** (udah ada prototype-nya) supaya nggak semua SKILL.md di-load ke context di awal — cuma di-scan metadata/description-nya, isi lengkap di-load pas match confidence tinggi. Ini prevention-style context management juga.

---

## 5. Plan-Execute Mode (buat task kompleks / codebase besar)

Trigger otomatis kalau:
- Task melibatkan >N file yang diperkirakan perlu diubah, ATAU
- User eksplisit minta "plan dulu" / "buatkan rencana", ATAU
- Eksplorasi awal (ReAct) nemuin scope lebih besar dari perkiraan awal (re-plan trigger).

Struktur plan disimpan sebagai structured data (bukan cuma teks bebas), biar bisa di-track progress-nya:

```go
type Plan struct {
    Goal  string
    Steps []PlanStep
}
type PlanStep struct {
    ID          string
    Description string
    Status      string // pending | in_progress | done | blocked | skipped
    Files       []string
}
```

UI nampilin ini sebagai checklist minimalis (bukan wall of text) — ini juga bagian dari "UI/UX minimalis elegan" yang lo mau: progress bar/checklist, bukan log scroll penuh.

---

## 6. UI/UX — 10% Effort, Tapi Effort yang Tepat

Prinsip: **tampilkan state, sembunyikan noise**. Yang WAJIB keliatan:
- Current phase (thinking / acting / verifying) — indikator kecil, bukan animasi berat.
- Tool call yang lagi jalan + hasil ringkas (collapsible, expand on demand — udah keliatan di transkrip lo pola `ctrl+o to view`, itu arah yang bener).
- Plan checklist (kalau lagi plan-execute mode).
- Token/context usage indicator (kecil, di corner) — biar user aware kapan compaction bakal/udah kejadian.

Yang JANGAN:
- Log mentah semua reasoning token streaming penuh ke layar (bikin noise, bukan clarity).
- Terlalu banyak warna/tema — Bubble Tea v2 gampang kebablasan dekorasi.
- Konfirmasi berlebihan buat aksi non-destruktif (baca file, grep — auto-approve, jangan tanya izin tiap kali).

---

## 7. Build Phases (Roadmap)

Selaras sama phased plan yang udah lo bikin, ini breakdown yang lebih granular fokus ke loop & context:

**Phase 0 — Skeleton**
- Provider & model config system (§10) — auto-detect + custom provider, minimal Poolside & DeepSeek jalan.
- Provider adapter (native tool calling), sqlite event log, CLI skeleton Bubble Tea v2.
- 1 tool doang (`bash`), loop paling sederhana, no compaction. Tujuan: pipeline end-to-end jalan.

*Definition of Done Phase 0:* `brocode` bisa start, auto-detect minimal 1 provider dari env var, kirim 1 pesan ke model, terima native tool_call, eksekusi `bash`, hasil kebaca balik ke model, model kasih jawaban akhir. Semua tanpa crash, semua ke-log di sqlite. **Repo skeleton awal:**

```
bro-code/
├── cmd/brocode/main.go
├── internal/
│   ├── loop/           # state machine, AgentTurn, verification (§2)
│   ├── context/         # event log, compactor, memory (§3)
│   ├── tool/            # tool registry + built-in tools (§4)
│   ├── provider/         # provider registry, adapters, auto-detect (§10)
│   ├── mcp/              # MCP client (Phase 4)
│   ├── skill/             # SKILL.md loader + fuzzy matcher (udah ada prototype)
│   ├── plan/               # Plan-Execute mode (§5)
│   ├── ui/                  # Bubble Tea v2 views (§6)
│   └── store/                # sqlite schema + migrations
├── docs/
│   └── NEW_GENERATION_BROCODE.md   # dokumen ini
├── AGENTS.md
└── go.mod
```

**Phase 1 — Core ReAct Loop**
- Tambah tool set minimal (§4.2).
- Implementasi `AgentTurn` dengan `Reasoning` wajib (§2.2).
- Loop continuation rule (§2.3) — ini yang fix masalah "berhenti kecepetan".
- Verification level 1 & 2 (§2.4).

*DoD:* dikasih task realistis ("cari & jelasin flow X di codebase"), agent chaining tool call sendiri (grep → read → read lagi) tanpa nanya balik user di tengah, sampai bener-bener butuh keputusan user. Nggak ada jawaban "biasanya/mungkin" tanpa grounding ke tool result.

**Phase 2 — Context Engineering**
- Prevention-style tool output limiting (§3.1.1).
- Dynamic discovery buat file besar (§3.1.2).
- Reactive compaction + structured summary (§3.1.3-4).
- `/debug-context` command buat inspect apa yang beneran dikirim ke API.

*DoD:* session 50+ turn di codebase besar nggak overflow context, compaction trigger kelihatan di UI, `/debug-context` nunjukin persis apa yang dikirim (bukan black box).

**Phase 3 — Planning & Multi-file**
- Plan-Execute mode (§5).
- Generate-Test-Repair sub-loop (auto-detect test command, loop terbatas).
- Verification level 3 & 4.

*DoD:* task multi-file (>3 file) otomatis masuk plan mode, checklist keliatan progress-nya, test runner ke-detect otomatis & re-run pas ada perubahan logic.

**Phase 4 — Extensibility**
- MCP client.
- SKILL.md fuzzy loader integration.
- Sub-agent spawning (`task` tool) buat isolasi context per sub-task.
- chromem-go long-term memory (§3.3).

*DoD:* connect ke 1 MCP server eksternal jalan, skill match fuzzy kepanggil otomatis tanpa exact keyword, sub-agent task selesai tanpa numpuk context sesi utama.

**Phase 5 — Polish & Efficiency**
- UI refinement (§6).
- Benchmark token efficiency vs opencode/Crush pakai prompt+model yang sama (buat validasi klaim "lebih efisien").
- Eval harness (lihat §8).

*DoD:* golden trace comparison vs opencode buat 3+ task realistis nunjukin token usage brocode ≤ opencode dengan hasil setara/lebih baik.

---

## 8. Testing & Eval — Jangan Skip Ini

Riset scaffold taxonomy negesin: reliability itu **architectural property**, bukan cuma soal model bagus. Cara buktiinnya:

1. **Golden trace tests**: rekam session dari opencode/Crush buat task yang sama, bandingin trace (jumlah tool call, token usage, keputusan yang diambil) vs brocode lo dengan model+prompt identik. Ini yang lo butuh buat jawab pertanyaan awal lo sendiri ("kenapa opencode lebih smart") secara empiris, bukan tebak-tebakan.
2. **Mock provider buat unit test loop logic**: loop state machine, compaction trigger, verification ladder — semua harus testable tanpa manggil LLM beneran.
3. **Regression suite kecil**: kumpulan task realistis (fix bug di file X, tambah fitur Y, refactor Z) dijalanin tiap ada perubahan besar ke harness, cek apakah makin baik atau makin jelek.

---

## 9. Anti-Pattern (dari riset + dari bug transkrip lo kemarin)

- ❌ Loop berhenti setelah 1 tool call tanpa alasan eksplisit → fix: §2.3.
- ❌ Tool result nggak di-ground ulang sebelum jawaban final (jawaban masih "mungkin"/"biasanya" padahal udah ada data) → fix: paksa `Reasoning` field wajib reference hasil tool terbaru sebelum `Answer`.
- ❌ Custom text-block tool-call format buat model yang dilatih native function calling → fix: §4.1.
- ❌ Compact context di tengah subgoal aktif → fix: §3.1 poin 5.
- ❌ Stuff seluruh isi file/tool output besar ke context tanpa filter → fix: §3.1 poin 1-2.
- ❌ Terlalu banyak tool (>30) bikin model bingung milih → fix: mulai minimal (§4.2), tambah cuma kalau ada bukti butuh.
- ❌ UI yang nampilin semua log mentah → noise, bukan clarity → fix: §6.

---

## 10. Provider & Model Configuration System

Ini bagian yang **diadopsi & dipertahankan dari brocode lama**, disesuaikan gaya opencode (auto-detect provider dari env, plus custom provider config). Tujuannya: user nggak perlu edit config manual kalau API key udah di-set di env, tapi tetep fleksibel buat provider custom/self-hosted (kayak Poolside).

### 10.1 Precedence Config

```
1. Flag CLI (--provider, --model)             ← paling prioritas, override sesi ini aja
2. Project config (.brocode/config.json)       ← per-project default
3. Global config (~/.config/brocode/config.json) ← default lintas project
4. Auto-detect dari env var                    ← fallback kalau belum ada config sama sekali
5. Interactive setup wizard (first run)         ← kalau semua di atas kosong
```

### 10.2 Provider Registry (Built-in)

Static registry di `internal/provider/registry.go`, tiap entry punya: nama, protokol (`anthropic` | `openai-compatible`), base URL default, nama env var buat API key, dan cara ambil daftar model (static list atau fetch dari endpoint `/models`).

| Provider | Protocol | Env Var | Base URL default | Catatan |
|---|---|---|---|---|
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` | `api.anthropic.com` | native thinking support |
| OpenAI | `openai-compatible` | `OPENAI_API_KEY` | `api.openai.com/v1` | |
| DeepSeek | `openai-compatible` | `DEEPSEEK_API_KEY` | `api.deepseek.com` | fully OpenAI-compatible, murah buat dev loop |
| Poolside | `openai-compatible` | `POOLSIDE_API_KEY` | (custom, sesuai deployment — self-host vLLM/SGLang atau hosted endpoint mereka) | pakai `tool-call-parser poolside_v1` kalau self-host; kalau lewat hosted API-nya, biasanya udah OpenAI-compatible di permukaan — **cek dokumentasi endpoint mereka pas setup, jangan asumsi identik OpenAI 100%** |
| OpenRouter | `openai-compatible` | `OPENROUTER_API_KEY` | `openrouter.ai/api/v1` | akses banyak model termasuk laguna-s-2.1:free, model gratis lain |
| Groq | `openai-compatible` | `GROQ_API_KEY` | `api.groq.com/openai/v1` | buat subagent murah/cepat (Phase 4) |
| Google | `openai-compatible`* | `GOOGLE_API_KEY` / `GEMINI_API_KEY` | | *Gemini punya endpoint OpenAI-compat, bisa reuse adapter yang sama |
| Ollama (local) | `openai-compatible` | — (no key) | `localhost:11434/v1` | buat laguna-s-2.1 self-host, dev tanpa API cost |
| Custom | `openai-compatible` \| `anthropic` | user-defined | user-defined | via config, lihat §10.4 |

**Prinsip desain:** karena hampir semua provider di atas OpenAI-compatible, adapter cukup **1 implementasi generik `openai_compat.go`** + 1 implementasi khusus `anthropic.go` (buat native thinking/extended reasoning). Jangan bikin 1 file adapter per provider — itu duplikasi nggak perlu. Bedanya cuma base URL, auth header format, dan quirk kecil (tool-call parser Poolside kalau self-host).

### 10.3 Auto-Detect Flow (gaya opencode)

```go
func AutoDetect() []DetectedProvider {
    var found []DetectedProvider
    for _, p := range registry.BuiltinProviders {
        if key, ok := os.LookupEnv(p.APIKeyEnvVar); ok && key != "" {
            found = append(found, DetectedProvider{Provider: p, APIKey: key})
        }
    }
    return found
}
```

- Startup: scan semua env var yang dikenal registry. Kalau ketemu ≥1, langsung usable tanpa config file — user bisa langsung `brocode` dan milih model dari yang ke-detect.
- Kalau ketemu >1 provider, kasih picker interaktif (atau default ke urutan preferensi yang bisa di-config).
- Kalau 0 ketemu, masuk **first-run wizard**: tanya provider mana, API key (disimpan, bukan cuma env var — biar persist), test koneksi (1 ping call ringan) sebelum disimpan.

### 10.4 Custom Provider Config

Buat Poolside self-host atau endpoint lain yang nggak ada di built-in registry:

```jsonc
// .brocode/config.json atau ~/.config/brocode/config.json
{
  "providers": {
    "poolside-local": {
      "protocol": "openai-compatible",
      "base_url": "http://localhost:8000/v1",
      "api_key_env": "POOLSIDE_API_KEY",
      "models": [
        { "id": "laguna-s-2.1", "supports_thinking": true, "tool_call_style": "native" }
      ]
    },
    "my-company-proxy": {
      "protocol": "anthropic",
      "base_url": "https://llm-proxy.internal.company.com",
      "api_key_env": "COMPANY_LLM_KEY",
      "models": [{ "id": "claude-sonnet-4-6" }]
    }
  },
  "default_provider": "poolside-local",
  "default_model": "laguna-s-2.1"
}
```

Kalau `models` nggak diisi di config, coba fetch dari `{base_url}/models` (standar OpenAI-compat) sebagai fallback sebelum minta user isi manual.

### 10.5 API Key Storage

Jangan simpan API key plaintext di file yang gampang ke-commit. Aturan:
- Prioritas: baca dari env var tiap saat (paling aman, nggak persist di disk).
- Kalau user pilih "simpan" via wizard: tulis ke `~/.config/brocode/config.json` dengan permission `0600`, **bukan** di project-local config (biar nggak ke-commit ke git kalau lupa `.gitignore`).
- Tambahin `.brocode/` ke template `.gitignore` default yang di-generate pas init project.

### 10.6 Model Switching Mid-Session

Sama kayak opencode/Pi — user bisa `/model <provider>/<model>` di tengah sesi, context tetep kepreserve (event log nggak peduli model mana yang generate-nya). Ini penting buat workflow "explore pakai model murah/cepat, execute pakai model kuat" yang lo sering pakai.

---

## 11. Referensi Riset (buat bacaan lanjut)

- *Inside the Scaffold: A Source-Code Taxonomy of Coding Agent Architectures* — arXiv 2604.03515 (13 agent scaffold dibedah source-code level)
- *Stop Hand-Holding Your Coding Agent: Engineering the Loops that Replace Step-by-Step Prompting* — arXiv 2607.00038 (loop specification anatomy: trigger, goal, verification ladder, stopping rule, memory)
- *Self-Compacting Language Model Agents* — arXiv 2606.23525 (reactive vs periodic compaction, context rot)
- *Context Compaction Theory* — arXiv 2608.01326 (formal framework: Context Selection Game vs Context Generation Game)
- Breakdown teknis compaction Claude Code / Codex CLI / OpenCode / Gemini CLI / Cursor — x-cmd.com blog & Zylos Research (2026) — detail trigger threshold, structured summary format tiap agent
- Source code langsung: `github.com/sst/opencode` (session/compaction logic), `github.com/charmbracelet/crush` — buat golden trace comparison di §8.

---

*Catatan: dokumen ini asumsi kondisi riset per Agustus 2026. Compaction API provider (misal `compact-2026-01-12` dari Anthropic) terus berkembang — cek ulang kalau lo mau adopsi provider-native compaction daripada bikin sendiri.*
