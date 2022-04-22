package indexer

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sourcegraph/doctree/doctree/schema"
)

// Language describes an indexer for a specific language.
type Language interface {
	// Name of the language this indexer works for.
	Name() schema.Language

	// Extensions returns a list of file extensions commonly associated with the language.
	Extensions() []string

	// IndexDir indexes a directory of code likely to contain sources in this language recursively.
	IndexDir(ctx context.Context, dir string) (*schema.Index, error)
}

// Registered indexers by language ID ("go", "objc", "cpp", etc.)
var Registered = map[string]Language{}

// Registers a doctree language indexer.
func Register(indexer Language) {
	Registered[indexer.Name().ID] = indexer
}

// IndexDir indexes the specified directory recursively. It looks at the file extension of every
// file, and then asks the registered indexers for each language to index.
//
// Returns the successful indexes and any errors.
func IndexDir(ctx context.Context, dir string) (map[string]*schema.Index, error) {
	// Identify all file extensions in the directory recursively.
	extensions := map[string]struct{}{}
	if err := fs.WalkDir(os.DirFS(dir), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // error walking dir
		}
		ext := filepath.Ext(path)
		if ext != "" && ext != "." {
			ext = ext[1:] // ".txt" -> "txt"
			extensions[ext] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "WalkDir")
	}

	// Map extensions to indexers.
	indexersByExtension := map[string][]Language{}
	for _, language := range Registered {
		for _, ext := range language.Extensions() {
			indexers := indexersByExtension[ext]
			indexers = append(indexers, language)
			indexersByExtension[ext] = indexers
		}
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.Wrap(err, "Abs")
	}

	// Run indexers for each language.
	var (
		wg sync.WaitGroup

		mu      sync.Mutex
		errs    error
		results = map[string]*schema.Index{}
	)
	// TODO: configurable parallelism?
	for ext := range extensions {
		ext := ext
		for _, indexer := range indexersByExtension[ext] {
			indexer := indexer
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				index, err := indexer.IndexDir(ctx, dir)
				if index != nil {
					index.DurationSeconds = time.Since(start).Seconds()
					index.CreatedAt = time.Now().Format(time.RFC3339)
					index.Directory = absDir
				}

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = multierror.Append(errs, err)
				} else {
					results[indexer.Name().ID] = index
				}
			}()
		}
	}
	wg.Wait()
	return results, errs
}

// WriteIndexes writes indexes to the index directory:
//
// index/<index_id>/<language_id>
func WriteIndexes(indexedDir, indexDataDir string, indexes map[string]*schema.Index) error {
	// TODO: binary format?
	// TODO: compression

	// Ensure paths are absolute first. Index ID is absolute path of indexed directory effectively.
	var err error
	indexDataDir, err = filepath.Abs(indexDataDir)
	if err != nil {
		return errors.Wrap(err, "Abs")
	}
	indexedDir, err = filepath.Abs(indexedDir)
	if err != nil {
		return errors.Wrap(err, "Abs")
	}

	outDir := filepath.Join(indexDataDir, pathToIndexID(indexedDir))

	// Delete any old index data in this dir (e.g. if we had python+go before, but now only go, we
	// need to delete python index.)
	if err := os.RemoveAll(outDir); err != nil {
		return errors.Wrap(err, "RemoveAll")
	}
	if err := os.MkdirAll(outDir, os.ModePerm); err != nil {
		return errors.Wrap(err, "MkdirAll")
	}

	for lang, index := range indexes {
		f, err := os.Create(filepath.Join(outDir, lang))
		if err != nil {
			return errors.Wrap(err, "Create")
		}
		defer f.Close()

		if err := json.NewEncoder(f).Encode(index); err != nil {
			return errors.Wrap(err, "Encode")
		}
	}
	return nil
}

func pathToIndexID(path string) string {
	return strings.ReplaceAll(strings.Trim(path, "/"), "/", "-")
}
