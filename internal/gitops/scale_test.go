package gitops

import (
	"fmt"
	"testing"
	"time"
)

// 17,845 stub files is the size of a real schema (spec §16): init must stay fast.
func TestScaleCreateBranch(t *testing.T) {
	r, _ := newMirror(t)
	in := map[string][]byte{}
	for i := 0; i < 17845; i++ {
		in[fmt.Sprintf("procedures/PROC_%05d.prc", i)] = []byte(fmt.Sprintf("-- OVC:STUB type=PROCEDURE %d\n", i))
	}
	start := time.Now()
	if _, err := r.CreateBranch(ctx, "big_dev", in, opts); err != nil {
		t.Fatal(err)
	}
	create := time.Since(start)
	start = time.Now()
	if err := r.Push(ctx, "big_dev"); err != nil {
		t.Fatal(err)
	}
	push := time.Since(start)
	start = time.Now()
	h, _ := r.Head(ctx, "big_dev")
	if _, err := r.Commit(ctx, "big_dev", h, []Change{{Path: "procedures/PROC_00042.prc", Content: []byte("real ddl\n")}}, opts); err != nil {
		t.Fatal(err)
	}
	one := time.Since(start)
	start = time.Now()
	fs, _ := r.Files(ctx, "big_dev")
	list := time.Since(start)
	t.Logf("create=%s push=%s single-file commit=%s list=%s files=%d", create.Round(time.Millisecond), push.Round(time.Millisecond), one.Round(time.Millisecond), list.Round(time.Millisecond), len(fs))
	if one > 2*time.Second {
		t.Errorf("a one-file commit took %s", one)
	}
}
