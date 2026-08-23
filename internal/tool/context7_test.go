package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContext7ClientMock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/search") {
			q := r.URL.Query().Get("query")
			if q == "nextjs" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"results": []map[string]any{
						{
							"id":          "/vercel/next.js",
							"name":        "Next.js",
							"description": "The React Framework for the Web",
						},
					},
				})
				return
			}
			http.NotFound(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/docs") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"markdown": "# Next.js Documentation\n\nNext.js enables you to create full-stack Web applications by extending the latest React features.",
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := &Context7Client{
		apiKey:     "test_c7_key",
		baseURL:    ts.URL,
		httpClient: ts.Client(),
	}

	ctx := context.Background()

	// 1. Test ResolveLibrary
	libID, err := client.ResolveLibrary(ctx, "nextjs")
	if err != nil {
		t.Fatalf("ResolveLibrary failed: %v", err)
	}
	if libID != "/vercel/next.js" {
		t.Fatalf("expected /vercel/next.js, got %q", libID)
	}

	// 2. Test GetDocs
	docs, err := client.GetDocs(ctx, libID, "routing")
	if err != nil {
		t.Fatalf("GetDocs failed: %v", err)
	}
	if !strings.Contains(docs, "Next.js enables you to create") {
		t.Fatalf("unexpected docs output: %s", docs)
	}
}

func TestDocLookupToolExecute(t *testing.T) {
	tool := &DocLookupTool{}
	if tool.Name() != "doc_lookup" {
		t.Fatalf("expected name doc_lookup, got %s", tool.Name())
	}

	// Empty library validation
	_, err := tool.Execute(context.Background(), `{"library": ""}`)
	if err == nil {
		t.Fatal("expected error for empty library")
	}
}
