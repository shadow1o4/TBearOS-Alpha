// Package graph is a minimal Microsoft Graph OneDrive client.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type Item struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Size                 int64     `json:"size"`
	ETag                 string    `json:"eTag"`
	CTag                 string    `json:"cTag"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
	Folder               *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder,omitempty"`
	File *struct {
		MimeType string `json:"mimeType"`
	} `json:"file,omitempty"`
	// Package items (e.g. OneNote notebooks) are neither files nor folders.
	Package *struct {
		Type string `json:"type"`
	} `json:"package,omitempty"`
	ParentReference *struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"parentReference,omitempty"`
}

func (i *Item) IsDir() bool { return i.Folder != nil }

type Quota struct {
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("graph: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func IsNotFound(err error) bool { return IsStatus(err, http.StatusNotFound) }
func IsConflict(err error) bool { return IsStatus(err, http.StatusConflict) }

// SimpleUploadMax is the largest file uploaded with a single PUT; larger
// files use an upload session.
const SimpleUploadMax = 4 << 20

// chunkSize must be a multiple of 320 KiB per the Graph documentation.
const chunkSize = 32 * 320 * 1024

type Client struct {
	Base   string
	Tokens TokenSource
	HTTP   *http.Client
}

func New(base string, ts TokenSource) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Tokens: ts, HTTP: &http.Client{Timeout: 0}}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = c.Base + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	ae := &APIError{Status: resp.StatusCode}
	if json.Unmarshal(b, &e) == nil && e.Error.Code != "" {
		ae.Code, ae.Message = e.Error.Code, e.Error.Message
	} else {
		ae.Message = strings.TrimSpace(string(b))
	}
	return ae
}

func doJSON(c *Client, ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, body, ct, out)
}

func itemPath(id string) string { return "/me/drive/items/" + url.PathEscape(id) }

func childPath(parentID, name string) string {
	return itemPath(parentID) + ":/" + url.PathEscape(name) + ":"
}

func (c *Client) Quota(ctx context.Context) (*Quota, error) {
	var d struct {
		Quota Quota `json:"quota"`
	}
	if err := doJSON(c, ctx, http.MethodGet, "/me/drive", nil, &d); err != nil {
		return nil, err
	}
	return &d.Quota, nil
}

func (c *Client) Root(ctx context.Context) (*Item, error) {
	var it Item
	return &it, doJSON(c, ctx, http.MethodGet, "/me/drive/root", nil, &it)
}

func (c *Client) Item(ctx context.Context, id string) (*Item, error) {
	var it Item
	return &it, doJSON(c, ctx, http.MethodGet, itemPath(id), nil, &it)
}

func (c *Client) Child(ctx context.Context, parentID, name string) (*Item, error) {
	var it Item
	return &it, doJSON(c, ctx, http.MethodGet, childPath(parentID, name), nil, &it)
}

func (c *Client) Children(ctx context.Context, id string) ([]Item, error) {
	var all []Item
	next := itemPath(id) + "/children?$top=1000"
	for next != "" {
		var page struct {
			Value    []Item `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := doJSON(c, ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		next = page.NextLink
	}
	return all, nil
}

func (c *Client) Download(ctx context.Context, id string, w io.Writer) error {
	return c.do(ctx, http.MethodGet, itemPath(id)+"/content", nil, "", w)
}

func (c *Client) Mkdir(ctx context.Context, parentID, name string) (*Item, error) {
	in := map[string]any{"name": name, "folder": map[string]any{}, "@microsoft.graph.conflictBehavior": "fail"}
	var it Item
	return &it, doJSON(c, ctx, http.MethodPost, itemPath(parentID)+"/children", in, &it)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, itemPath(id), nil, "", nil)
}

// Move renames and/or reparents an item.
func (c *Client) Move(ctx context.Context, id, newParentID, newName string) (*Item, error) {
	in := map[string]any{"name": newName, "parentReference": map[string]string{"id": newParentID}}
	var it Item
	return &it, doJSON(c, ctx, http.MethodPatch, itemPath(id), in, &it)
}

// Upload creates or replaces parentID/name with size bytes from r.
func (c *Client) Upload(ctx context.Context, parentID, name string, r io.ReaderAt, size int64) (*Item, error) {
	if size <= SimpleUploadMax {
		var it Item
		err := c.do(ctx, http.MethodPut, childPath(parentID, name)+"/content", io.NewSectionReader(r, 0, size), "application/octet-stream", &it)
		return &it, err
	}
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	in := map[string]any{"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"}}
	if err := doJSON(c, ctx, http.MethodPost, childPath(parentID, name)+"/createUploadSession", in, &sess); err != nil {
		return nil, err
	}
	for off := int64(0); off < size; off += chunkSize {
		n := min(chunkSize, size-off)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, sess.UploadURL, io.NewSectionReader(r, off, n))
		if err != nil {
			return nil, err
		}
		// The upload URL is pre-authenticated; it must not carry our token.
		req.ContentLength = n
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, size))
		resp, err := c.HTTP.Do(req)
		if err != nil {
			_ = c.cancelSession(sess.UploadURL)
			return nil, err
		}
		if resp.StatusCode >= 300 {
			err := decodeError(resp)
			resp.Body.Close()
			_ = c.cancelSession(sess.UploadURL)
			return nil, err
		}
		if off+n == size {
			var it Item
			err := json.NewDecoder(resp.Body).Decode(&it)
			resp.Body.Close()
			return &it, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return nil, errors.New("upload session ended without final item")
}

func (c *Client) cancelSession(u string) error {
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}
