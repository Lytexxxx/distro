package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ObjectStorage garde les pièces jointes et les images dans un bucket Supabase privé.
// La clé service_role reste exclusivement côté serveur.
type ObjectStorage struct {
	base   string
	key    string
	bucket string
	client *http.Client
}

func newObjectStorage() (*ObjectStorage, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("SUPABASE_URL")), "/")
	key := strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY"))
	bucket := strings.TrimSpace(os.Getenv("SUPABASE_STORAGE_BUCKET"))
	if base == "" || key == "" || bucket == "" {
		return nil, fmt.Errorf("SUPABASE_URL, SUPABASE_SERVICE_ROLE_KEY et SUPABASE_STORAGE_BUCKET sont obligatoires avec DATABASE_URL")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("SUPABASE_URL invalide")
	}
	return &ObjectStorage{base: base, key: key, bucket: bucket, client: &http.Client{Timeout: 90 * time.Second}}, nil
}

func (o *ObjectStorage) objectURL(path string) string {
	parts := strings.Split(path, "/")
	for i := range parts { parts[i] = url.PathEscape(parts[i]) }
	return o.base + "/storage/v1/object/" + url.PathEscape(o.bucket) + "/" + strings.Join(parts, "/")
}

func (o *ObjectStorage) request(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, o.objectURL(path), body)
	if err != nil { return nil, err }
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("apikey", o.key)
	if contentType != "" { req.Header.Set("Content-Type", contentType) }
	if method == http.MethodPost { req.Header.Set("x-upsert", "true") }
	return o.client.Do(req)
}

func (o *ObjectStorage) Upload(path string, data []byte, contentType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := o.request(ctx, http.MethodPost, path, contentType, bytes.NewReader(data))
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Supabase Storage upload (%s): %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (o *ObjectStorage) Download(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := o.request(ctx, http.MethodGet, path, "", nil)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Supabase Storage download (%s): %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(resp.Body)
}

func (o *ObjectStorage) Delete(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string][]string{"prefixes": {path}})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.base+"/storage/v1/object/"+url.PathEscape(o.bucket), bytes.NewReader(body))
	if err != nil { return err }
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("apikey", o.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Supabase Storage delete (%s): %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
