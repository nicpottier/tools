// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"go/token"
	"os"
	"sync"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/span"
)

type View struct {
	// mu protects all mutable state of the view.
	mu sync.Mutex

	// backgroundCtx is the current context used by background tasks initiated
	// by the view.
	backgroundCtx context.Context

	// cancel is called when all action being performed by the current view
	// should be stopped.
	cancel context.CancelFunc

	// Config is the configuration used for the view's interaction with the
	// go/packages API. It is shared across all views.
	Config packages.Config

	// keep track of files by uri and by basename, a single file may be mapped
	// to multiple uris, and the same basename may map to multiple files
	filesByURI  map[span.URI]*File
	filesByBase map[string][]*File

	// contentChanges saves the content changes for a given state of the view.
	// When type information is requested by the view, all of the dirty changes
	// are applied, potentially invalidating some data in the caches. The
	// closures  in the dirty slice assume that their caller is holding the
	// view's mutex.
	contentChanges map[span.URI]func()

	// mcache caches metadata for the packages of the opened files in a view.
	mcache *metadataCache

	// pcache caches type information for the packages of the opened files in a view.
	pcache *packageCache
}

type metadataCache struct {
	mu       sync.Mutex
	packages map[string]*metadata
}

type metadata struct {
	id, pkgPath, name string
	files             []string
	parents, children map[string]bool
}

type packageCache struct {
	mu       sync.Mutex
	packages map[string]*entry
}

type entry struct {
	pkg   *Package
	err   error
	ready chan struct{} // closed to broadcast ready condition
}

func NewView(config *packages.Config) *View {
	ctx, cancel := context.WithCancel(context.Background())

	return &View{
		backgroundCtx:  ctx,
		cancel:         cancel,
		Config:         *config,
		filesByURI:     make(map[span.URI]*File),
		filesByBase:    make(map[string][]*File),
		contentChanges: make(map[span.URI]func()),
		mcache: &metadataCache{
			packages: make(map[string]*metadata),
		},
		pcache: &packageCache{
			packages: make(map[string]*entry),
		},
	}
}

func (v *View) BackgroundContext() context.Context {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.backgroundCtx
}

func (v *View) FileSet() *token.FileSet {
	return v.Config.Fset
}

// SetContent sets the overlay contents for a file.
func (v *View) SetContent(ctx context.Context, uri span.URI, content []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Cancel all still-running previous requests, since they would be
	// operating on stale data.
	v.cancel()
	v.backgroundCtx, v.cancel = context.WithCancel(context.Background())

	v.contentChanges[uri] = func() {
		v.applyContentChange(uri, content)
	}

	return nil
}

// applyContentChanges applies all of the changed content stored in the view.
// It is assumed that the caller has locked both the view's and the mcache's
// mutexes.
func (v *View) applyContentChanges(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	v.pcache.mu.Lock()
	defer v.pcache.mu.Unlock()

	for uri, change := range v.contentChanges {
		change()
		delete(v.contentChanges, uri)
	}

	return nil
}

// setContent applies a content update for a given file. It assumes that the
// caller is holding the view's mutex.
func (v *View) applyContentChange(uri span.URI, content []byte) {
	f, err := v.getFile(uri)
	if err != nil {
		return
	}
	f.content = content

	// TODO(rstambler): Should we recompute these here?
	f.ast = nil
	f.token = nil

	// Remove the package and all of its reverse dependencies from the cache.
	if f.pkg != nil {
		v.remove(f.pkg.pkgPath)
	}

	switch {
	case f.active && content == nil:
		// The file was active, so we need to forget its content.
		f.active = false
		delete(f.view.Config.Overlay, f.filename)
		f.content = nil
	case content != nil:
		// This is an active overlay, so we update the map.
		f.active = true
		f.view.Config.Overlay[f.filename] = f.content
	}
}

// remove invalidates a package and its reverse dependencies in the view's
// package cache. It is assumed that the caller has locked both the mutexes
// of both the mcache and the pcache.
func (v *View) remove(pkgPath string) {
	m, ok := v.mcache.packages[pkgPath]
	if !ok {
		return
	}
	for parentPkgPath := range m.parents {
		v.remove(parentPkgPath)
	}
	// All of the files in the package may also be holding a pointer to the
	// invalidated package.
	for _, filename := range m.files {
		if f := v.findFile(span.FileURI(filename)); f != nil {
			f.pkg = nil
		}
	}
	delete(v.pcache.packages, pkgPath)
}

// GetFile returns a File for the given URI. It will always succeed because it
// adds the file to the managed set if needed.
func (v *View) GetFile(ctx context.Context, uri span.URI) (source.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return v.getFile(uri)
}

// getFile is the unlocked internal implementation of GetFile.
func (v *View) getFile(uri span.URI) (*File, error) {
	if f := v.findFile(uri); f != nil {
		return f, nil
	}
	filename, err := uri.Filename()
	if err != nil {
		return nil, err
	}
	f := &File{
		view:     v,
		filename: filename,
	}
	v.mapFile(uri, f)
	return f, nil
}

func (v *View) findFile(uri span.URI) *File {
	if f := v.filesByURI[uri]; f != nil {
		// a perfect match
		return f
	}
	// no exact match stored, time to do some real work
	// check for any files with the same basename
	fname, err := uri.Filename()
	if err != nil {
		return nil
	}
	basename := basename(fname)
	if candidates := v.filesByBase[basename]; candidates != nil {
		pathStat, err := os.Stat(fname)
		if err == nil {
			return nil
		}
		for _, c := range candidates {
			if cStat, err := os.Stat(c.filename); err == nil {
				if os.SameFile(pathStat, cStat) {
					// same file, map it
					v.mapFile(uri, c)
					return c
				}
			}
		}
	}
	return nil
}

func (v *View) mapFile(uri span.URI, f *File) {
	v.filesByURI[uri] = f
	f.uris = append(f.uris, uri)
	if f.basename == "" {
		f.basename = basename(f.filename)
		v.filesByBase[f.basename] = append(v.filesByBase[f.basename], f)
	}
}
