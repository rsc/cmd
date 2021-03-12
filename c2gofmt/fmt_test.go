// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"testing"

	"rsc.io/rf/diff"
)

func Test(t *testing.T) {
	parseRules("builtin rules", `
		XMethod(x, y) -> x.Method(y)
		r.min -> r.Min
		r.max -> r.Max
		p.x -> p.X
		p.y -> p.Y
	`)

	files, _ := filepath.Glob("testdata/*.txt")
	if len(files) == 0 {
		t.Fatalf("no testdata")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := ioutil.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			i := bytes.Index(data, []byte("\n---\n"))
			if i < 0 {
				t.Fatalf("cannot find --- marker")
			}
			cdata, want := data[:i+1], data[i+5:]
			have := do(file, cdata)
			if !bytes.Equal(have, want) {
				t.Errorf("%s:\n%s", file, have)
				t.Errorf("want:\n%s", want)
				d, err := diff.Diff("want", want, "have", have)
				if err == nil {
					t.Errorf("diff:\n%s", d)
				}
			}
		})
	}
}
