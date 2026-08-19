package stats

import "testing"

func TestParseSchemaSizes(t *testing.T) {
	raw := "shop_example_com_b7oa\t14827520\nblog_example_com_a1c2\t524288\n"
	sizes := parseSchemaSizes(raw)
	if sizes["shop_example_com_b7oa"] != 14 {
		t.Errorf("shop size = %d MB, want 14", sizes["shop_example_com_b7oa"])
	}
	if sizes["blog_example_com_a1c2"] != 0 {
		t.Errorf("blog size = %d MB, want 0 (rounds down from 0.5 MB)", sizes["blog_example_com_a1c2"])
	}
}

// A schema with no tables yet (a site just created, before WordPress
// installs) reports NULL for SUM(), not a number; that must read as a
// missing entry, not corrupt the whole batch.
func TestParseSchemaSizesHandlesNull(t *testing.T) {
	raw := "empty_schema\tNULL\nreal_schema\t1048576\n"
	sizes := parseSchemaSizes(raw)
	if _, ok := sizes["empty_schema"]; ok {
		t.Error("a NULL sum should not produce a map entry")
	}
	if sizes["real_schema"] != 1 {
		t.Errorf("real_schema size = %d MB, want 1", sizes["real_schema"])
	}
}

func TestParseSchemaSizesEmpty(t *testing.T) {
	if sizes := parseSchemaSizes(""); len(sizes) != 0 {
		t.Errorf("expected an empty map, got %v", sizes)
	}
}
