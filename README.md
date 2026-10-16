# go-learn

Personal, adaptive audio courses for Go web apps: pick any topic and listen to a never-ending stream of
natural-sounding lessons that adapts to what you know. Type or speak at any point and what comes next changes.

Sibling to [go-draw](https://github.com/anchoo2kewl/go-draw), [go-wiki](https://github.com/anchoo2kewl/go-wiki)
and [go-api](https://github.com/anchoo2kewl/go-api).

- **Text from your subscriptions.** Lessons are written by the logged-in `claude` and `codex` CLIs run headless,
  not by API keys.
- **Self-hosted voice.** Speech and transcription run in a local MLX sidecar (`sidecar/`) on Apple Silicon.
  No paid voice APIs.
- **One engine, two roles.** A host that runs the models generates and steers; a host without them can mirror
  published courses for listen-only sharing.

Status: running daily. Design and progress: issue #1.

## Use it

```go
eng, _ := learn.New(ctx, db, // *sql.DB, SQLite dialect; go-learn makes its own learn_* tables
	learn.WithDataDir(dir),
	learn.WithIdentity(func(r *http.Request) (string, bool) { /* who is asking */ }),
	learn.WithVoice(vc, vc), // vc := &voice.Client{URL: "http://127.0.0.1:8791"} (sidecar/sidecar.py)
	learn.WithResearch([]string{"/path/to/code"}), // optional, read-only
)
go eng.Run(ctx)
mux.Handle("/learn/", http.StripPrefix("/learn", eng.Handler()))
```

The speech sidecar: `cd sidecar && uv venv --python 3.12 .venv && uv pip install --python .venv/bin/python mlx-audio parakeet-mlx misaki soundfile`,
then `.venv/bin/python sidecar.py` (or let the host supervise it).

## Licence

MIT
