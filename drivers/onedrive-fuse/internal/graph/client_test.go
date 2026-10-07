package graph_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph/graphtest"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func newClient(t *testing.T) (*graphtest.Server, *graph.Client) {
	srv := graphtest.New()
	t.Cleanup(srv.Close)
	return srv, graph.New(srv.GraphURL(), staticToken(srv.IssueToken()))
}

func TestChildrenPaging(t *testing.T) {
	srv, c := newClient(t)
	srv.PageSize = 2
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		srv.Put("dir/"+n, []byte(n))
	}
	ctx := context.Background()
	root, err := c.Root(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.Child(ctx, root.ID, "dir")
	if err != nil || !dir.IsDir() {
		t.Fatalf("Child(dir) = %+v, %v", dir, err)
	}
	kids, err := c.Children(ctx, dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 5 {
		t.Fatalf("got %d children, want 5", len(kids))
	}
}

func TestUploadDownloadMoveDelete(t *testing.T) {
	srv, c := newClient(t)
	ctx := context.Background()
	small := []byte("hello world")
	large := bytes.Repeat([]byte("0123456789abcdef"), (graph.SimpleUploadMax*3)/16+7) // ~12 MiB, 2 chunks
	for name, data := range map[string][]byte{"small.txt": small, "large name #1.bin": large} {
		it, err := c.Upload(ctx, "root", name, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		if it.Size != int64(len(data)) {
			t.Fatalf("%s size %d, want %d", name, it.Size, len(data))
		}
		var buf bytes.Buffer
		if err := c.Download(ctx, it.ID, &buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("%s: downloaded content differs", name)
		}
	}
	dir, err := c.Mkdir(ctx, "root", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Mkdir(ctx, "root", "folder"); !graph.IsConflict(err) {
		t.Fatalf("duplicate mkdir err = %v, want conflict", err)
	}
	it, _ := c.Child(ctx, "root", "small.txt")
	if _, err := c.Move(ctx, it.ID, dir.ID, "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if got, ok := srv.Get("folder/renamed.txt"); !ok || string(got) != "hello world" {
		t.Fatalf("moved file content = %q, %v", got, ok)
	}
	if err := c.Delete(ctx, dir.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Item(ctx, dir.ID); !graph.IsNotFound(err) {
		t.Fatalf("after delete err = %v, want not found", err)
	}
	if err := graph.New(srv.GraphURL(), staticToken("bad")).Delete(ctx, "root"); !graph.IsStatus(err, 401) || !strings.Contains(err.Error(), "InvalidAuthenticationToken") {
		t.Fatalf("bad token err = %v", err)
	}
}
