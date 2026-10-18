package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Polign/recall/backend/mongodb"
)

// The front door's key store needs only Put, List and Get, so any backend
// with Get can hold it. This runs its lifecycle on the MongoDB server
// RECALL_MONGODB_URL names, and skips without one.
func TestKeyStoreOnMongoDB(t *testing.T) {
	url := os.Getenv("RECALL_MONGODB_URL")
	if url == "" {
		t.Skip("set RECALL_MONGODB_URL to run against MongoDB")
	}
	b, err := mongodb.New(mongodb.Config{URL: url, Database: fmt.Sprintf("recall_keys_%d", time.Now().UnixNano()%1e12)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Database().Drop(context.Background()); b.Close() })
	ctx := t.Context()
	k := newKeyStore(b, "memories", 0)
	token, rec, err := k.create(ctx, "acme:sandbox-1", "sandbox-1", "sandbox-1", "test", false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := k.Authenticate(ctx, token); err != nil || p.Namespace != "acme:sandbox-1" || p.KeyID != rec.ID {
		t.Fatalf("principal %+v, %v", p, err)
	}
	if _, err := k.Authenticate(ctx, "rcl_"+rec.ID+"_notthesecret"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	keys, err := k.list(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list %+v, %v", keys, err)
	}
	if err := k.revoke(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	k.forget(rec.ID)
	if _, err := k.Authenticate(ctx, token); err == nil {
		t.Fatal("revoked key accepted")
	}
}
