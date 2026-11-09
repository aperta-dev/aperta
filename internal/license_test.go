// SPDX-License-Identifier: FSL-1.1-ALv2

package internal

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const spdx = "// SPDX-License-Identifier: FSL-1.1-ALv2"

func TestEveryGoFileHasTheLicenseLine(t *testing.T) {
	root := ".."
	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".empryo", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		if !sc.Scan() || sc.Text() != spdx || !sc.Scan() || sc.Text() != "" {
			rel, _ := filepath.Rel(root, path)
			missing = append(missing, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("these files must start with %q and a blank line:\n  %s", spdx, strings.Join(missing, "\n  "))
	}
}
