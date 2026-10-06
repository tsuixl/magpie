package fonts

import (
	"math"
	"reflect"
	"testing"
)

func TestGroupInstalledStyles(t *testing.T) {
	regular := Face{Family: "HarmonyOS Sans SC", Name: "Regular", Weight: 400, Style: "normal", Stretch: 100}
	medium := regular
	medium.Name, medium.Weight = "Medium", 500
	italic := regular
	italic.Name, italic.Style = "Italic", "italic"
	chinese := Face{Family: "思源黑体", Name: "Regular", Weight: 400, Style: "normal", Stretch: 100}
	got := group([]Face{chinese, medium, italic, regular, medium, {Family: "invalid"}})
	want := []Family{{Name: regular.Family, Styles: []Face{italic, regular, medium}}, {Name: chinese.Family, Styles: []Face{chinese}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("installed families and styles = %#v, want %#v", got, want)
	}
	if got := group(nil); got == nil || len(got) != 0 {
		t.Fatalf("an empty successful collection must be [], got %#v", got)
	}
}

func TestValidateChoice(t *testing.T) {
	f := Face{Family: `Reader's "中文"`, Name: "Medium", Weight: 500, Style: "normal", Stretch: 100}
	if err := Validate(&f); err != nil {
		t.Fatal(err)
	}
	if err := Validate(nil); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Face){
		func(f *Face) { f.Family = "" }, func(f *Face) { f.Name = "\x00" },
		func(f *Face) { f.Weight = 0 }, func(f *Face) { f.Weight = 1001 },
		func(f *Face) { f.Style = "bold" }, func(f *Face) { f.Stretch = -1 },
		func(f *Face) { f.Weight = math.NaN() }, func(f *Face) { f.Stretch = math.Inf(1) },
	} {
		bad := f
		change(&bad)
		if Validate(&bad) == nil {
			t.Fatalf("accepted invalid font %#v", bad)
		}
	}
}

func TestVariableTraitsKeepCSSPrecision(t *testing.T) {
	for _, width := range []float64{0, 25, 87.5, 250} {
		f := Face{Family: "Variable Sans", Name: "Book", Weight: 425.5, Style: "normal", Stretch: width}
		if err := Validate(&f); err != nil {
			t.Fatal(err)
		}
		got := group([]Face{f})
		if len(got) != 1 || len(got[0].Styles) != 1 || got[0].Styles[0] != f {
			t.Fatalf("lost variable traits: %#v", got)
		}
	}
}

func TestInstalledFonts(t *testing.T) {
	if !Available {
		t.Skip("this build has no desktop font API")
	}
	families, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) == 0 {
		t.Fatal("the system font collection is empty")
	}
	count := 0
	for _, family := range families {
		for _, face := range family.Styles {
			if face.Family != family.Name {
				t.Fatalf("wrong family: %#v", face)
			}
			if err := Validate(&face); err != nil {
				t.Fatalf("%#v: %v", face, err)
			}
			count++
		}
	}
	t.Logf("read %d installed families and %d styles", len(families), count)
}
