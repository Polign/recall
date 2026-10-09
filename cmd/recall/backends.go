package main

// The storage backends -backend can name. Each registers itself with
// recall.RegisterBackend when imported; adding a backend is one line here.
import (
	_ "github.com/Polign/recall/backend/qdrant"
	_ "github.com/Polign/recall/polign"
)
