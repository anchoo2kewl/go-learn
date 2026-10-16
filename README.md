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

Status: design. See the design issue.

## Licence

MIT
