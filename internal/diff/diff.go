package diff

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/kopialogging"
	"github.com/kopia/repo"
	"github.com/kopia/repo/object"
)

var log = kopialogging.Logger("diff")

// Comparer outputs diff information between two filesystems.
type Comparer struct {
	rep    *repo.Repository
	out    io.Writer
	tmpDir string

	DiffCommand   string
	DiffArguments []string
}

// Compare compares two filesystem entries and emits their diff information.
func (c *Comparer) Compare(ctx context.Context, e1, e2 fs.Entry) error {
	return c.compareEntry(ctx, e1, e2, ".")
}

// Close removes all temporary files used by the comparer.
func (c *Comparer) Close() error {
	return os.RemoveAll(c.tmpDir)
}

func (c *Comparer) compareDirectories(ctx context.Context, dir1, dir2 fs.Directory, parent string) error {
	log.Debugf("comparing directories %v", parent)
	var entries1, entries2 fs.Entries
	var err error

	if dir1 != nil {
		entries1, err = dir1.Readdir(ctx)
		if err != nil {
			return fmt.Errorf("unable to read first directory %v: %v", parent, err)
		}
	}

	if dir2 != nil {
		entries2, err = dir2.Readdir(ctx)
		if err != nil {
			return fmt.Errorf("unable to read second directory %v: %v", parent, err)
		}
	}

	return c.compareDirectoryEntries(ctx, entries1, entries2, parent)
}

// nolint:gocyclo
func (c *Comparer) compareEntry(ctx context.Context, e1, e2 fs.Entry, path string) error {
	// see if we have the same object IDs, which implies identical objects, thanks to content-addressable-storage
	if h1, ok := e1.(object.HasObjectID); ok {
		if h2, ok := e2.(object.HasObjectID); ok {
			if h1.ObjectID() == h2.ObjectID() {
				log.Debugf("unchanged %v", path)
				return nil
			}
		}
	}

	if e1 == nil {
		if dir2, isDir2 := e2.(fs.Directory); isDir2 {
			c.output("added directory %v\n", path)
			return c.compareDirectories(ctx, nil, dir2, path)
		}

		c.output("added file %v (%v bytes)\n", path, e2.Size())
		if f, ok := e2.(fs.File); ok {
			if err := c.compareFiles(ctx, nil, f, path); err != nil {
				return err
			}
		}
		return nil
	}

	if e2 == nil {
		if dir1, isDir1 := e1.(fs.Directory); isDir1 {
			c.output("removed directory %v\n", path)
			return c.compareDirectories(ctx, dir1, nil, path)
		}

		c.output("removed file %v (%v bytes)\n", path, e1.Size())
		if f, ok := e1.(fs.File); ok {
			if err := c.compareFiles(ctx, f, nil, path); err != nil {
				return err
			}
		}
		return nil
	}

	dir1, isDir1 := e1.(fs.Directory)
	dir2, isDir2 := e2.(fs.Directory)
	if isDir1 {
		if !isDir2 {
			// right is a non-directory, left is a directory
			c.output("changed %v from directory to non-directory\n", path)
			return nil
		}

		return c.compareDirectories(ctx, dir1, dir2, path)
	}

	if isDir2 {
		// left is non-directory, right is a directory
		log.Infof("changed %v from non-directory to a directory", path)
		return nil
	}

	c.output("changed %v at %v (size %v -> %v)\n", path, e2.ModTime().String(), e1.Size(), e2.Size())
	if f1, ok := e1.(fs.File); ok {
		if f2, ok := e2.(fs.File); ok {
			if err := c.compareFiles(ctx, f1, f2, path); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Comparer) compareDirectoryEntries(ctx context.Context, entries1, entries2 fs.Entries, dirPath string) error {
	e1byname := map[string]fs.Entry{}
	for _, e1 := range entries1 {
		e1byname[e1.Name()] = e1
	}

	for _, e2 := range entries2 {
		entryName := e2.Name()
		if err := c.compareEntry(ctx, e1byname[entryName], e2, dirPath+"/"+entryName); err != nil {
			return fmt.Errorf("error comparing %v: %v", entryName, err)
		}
		delete(e1byname, entryName)
	}

	// at this point e1byname only has entries present in entries1 but not entries2, those are the deleted ones
	for _, e1 := range entries1 {
		entryName := e1.Name()
		if _, ok := e1byname[entryName]; ok {
			if err := c.compareEntry(ctx, e1, nil, dirPath+"/"+entryName); err != nil {
				return fmt.Errorf("error comparing %v: %v", entryName, err)
			}
		}
	}

	return nil
}

func (c *Comparer) compareFiles(ctx context.Context, f1, f2 fs.File, fname string) error {
	if c.DiffCommand == "" {
		return nil
	}

	oldName := "/dev/null"
	newName := "/dev/null"

	if f1 != nil {
		oldName = filepath.Clean("old/" + fname)
		oldFile := filepath.Join(c.tmpDir, oldName)

		if err := c.downloadFile(ctx, f1, oldFile); err != nil {
			return fmt.Errorf("error downloading old file: %v", err)
		}

		defer os.Remove(oldFile) //nolint:errcheck
	}

	if f2 != nil {
		newName = filepath.Clean("new/" + fname)
		newFile := filepath.Join(c.tmpDir, newName)

		if err := c.downloadFile(ctx, f2, newFile); err != nil {
			return fmt.Errorf("error downloading new file: %v", err)
		}
		defer os.Remove(newFile) //nolint:errcheck
	}

	var args []string
	args = append(args, c.DiffArguments...)
	args = append(args, oldName)
	args = append(args, newName)

	cmd := exec.CommandContext(ctx, c.DiffCommand, args...)
	cmd.Dir = c.tmpDir
	cmd.Stdout = c.out
	cmd.Stderr = c.out
	cmd.Run() //nolint:errcheck
	return nil
}

func (c *Comparer) downloadFile(ctx context.Context, f fs.File, fname string) error {
	if err := os.MkdirAll(filepath.Dir(fname), 0700); err != nil {
		return err
	}

	src, err := f.Open(ctx)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck

	dst, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck

	_, err = io.Copy(dst, src)
	return err
}

func (c *Comparer) output(msg string, args ...interface{}) {
	fmt.Fprintf(c.out, msg, args...) //nolint:errcheck
}

// NewComparer creates a comparer for a given repository that will output the results to a given writer.
func NewComparer(rep *repo.Repository, out io.Writer) (*Comparer, error) {
	tmp, err := ioutil.TempDir("", "kopia")
	if err != nil {
		return nil, err
	}

	return &Comparer{rep: rep, out: out, tmpDir: tmp}, nil
}