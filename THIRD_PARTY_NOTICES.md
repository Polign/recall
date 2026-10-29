# Third-party notices

The recall binary includes, or downloads on first use, the following
third-party software and models. Each keeps its own license.

## Built into the binary

- **hugot** (github.com/knights-analytics/hugot), Apache License 2.0. Runs the
  built-in embedding model.
- **GoMLX** (github.com/gomlx/gomlx, github.com/gomlx/compute,
  github.com/gomlx/compute-onnx, github.com/gomlx/go-huggingface), Apache
  License 2.0. The pure-Go engine hugot runs models on.

Other Go module dependencies and their licenses are listed in `go.mod`;
run `go-licenses report ./cmd/recall` for the full list.

## Downloaded on first use

- **all-MiniLM-L6-v2** (sentence-transformers, ONNX export by Xenova,
  huggingface.co/Xenova/all-MiniLM-L6-v2 at revision
  751bff37182d3f1213fa05d7196b954e230abad9), Apache License 2.0. Downloaded
  into `~/.config/polign/recall/models` when the built-in embedder is first
  used; not redistributed with Recall.

## Polign software downloaded on first use

- **polign_db** (`polign-server`, github.com/Polign/polign releases), Polign's
  own database, under its own license, which is not Apache-2.0. Downloaded
  into `bin/` under Recall's home with its license beside it as
  `polign_db-LICENSE` when the local database is first used and no
  `polign-server` is installed; not redistributed with Recall. Set
  `RECALL_NO_DOWNLOAD=1` to refuse the download.
