package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const picoBuild = "pico.jade.min.css"

func TestNoAccentOverridesInAppCSS(t *testing.T) {
	b, err := FS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`--pico-primary[\w-]*\s*:`).FindString(string(b)); m != "" {
		t.Errorf("app.css overrides the accent (%q); accent values belong in accents.css", m)
	}
}

func TestBaseLinksChosenPicoBuild(t *testing.T) {
	b, err := FS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "/static/"+picoBuild) {
		t.Errorf("base.html does not link %s", picoBuild)
	}
	if strings.Contains(string(b), "/static/pico.min.css") {
		t.Error("base.html still links the default pico.min.css")
	}
	if _, err := FS.ReadFile("static/" + picoBuild); err != nil {
		t.Errorf("%s not bundled: %v", picoBuild, err)
	}
}

// Characterization (#36): the accent cascade in accents.css copies how Pico sets --pico-primary,
// once for light, and twice for dark (follow the device, and forced).
func TestPicoPrimaryOnlyInThemeBlocks(t *testing.T) {
	b, err := FS.ReadFile("static/" + picoBuild)
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if n := strings.Count(css, "--pico-primary:"); n != 3 {
		t.Fatalf("--pico-primary: declared %d times, want 3", n)
	}
	for _, want := range []string{
		":host(:not([data-theme=dark])),:root:not([data-theme=dark]),[data-theme=light]{color-scheme:light;",
		"@media only screen and (prefers-color-scheme:dark){:host(:not([data-theme])),:root:not([data-theme]){color-scheme:dark;",
		"[data-theme=dark]{color-scheme:dark;",
		"--pico-primary:#007a50",
		"--pico-primary:#00b478",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("pico.jade.min.css lacks %q", want)
		}
	}
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := FS.ReadFile("static/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var accentBlockRe = regexp.MustCompile(`\[data-accent="([a-z]+)"\]\s*\{([^}]*)\}`)

func accentVars(t *testing.T) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, m := range accentBlockRe.FindAllStringSubmatch(readStatic(t, "accents.css"), -1) {
		vars := map[string]string{}
		for _, v := range regexp.MustCompile(`(--accent-[a-z-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(m[2], -1) {
			vars[v[1]] = strings.TrimSpace(v[2])
		}
		out[m[1]] = vars
	}
	return out
}

func lum(r, g, b float64) float64 {
	f := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*f(r) + 0.7152*f(g) + 0.0722*f(b)
}

func hexLum(t *testing.T, h string) float64 {
	t.Helper()
	if len(h) == 4 {
		h = "#" + string([]byte{h[1], h[1], h[2], h[2], h[3], h[3]})
	}
	if len(h) != 7 || h[0] != '#' {
		t.Fatalf("not a hex color: %q", h)
	}
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(h[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		c[i] = float64(v)
	}
	return lum(c[0], c[1], c[2])
}

func ratio(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

var (
	lightPage  = lum(255, 255, 255)
	lightFoot  = lum(251, 251.5, 252.25)
	darkPage   = lum(19, 22.5, 30.5)
	darkCard   = lum(24, 28, 37)
	darkFooter = lum(26, 30.5, 40.25)
)

func TestAccentContrastAA(t *testing.T) {
	accents := accentVars(t)
	if len(accents) != 16 {
		t.Fatalf("%d accent blocks, want 16", len(accents))
	}
	for name, v := range accents {
		for _, k := range []string{"background", "inverse", "light-primary", "light-underline", "light-hover", "light-hover-background",
			"light-focus", "light-selection", "dark-primary", "dark-underline", "dark-hover", "dark-hover-background", "dark-focus", "dark-selection"} {
			if v["--accent-"+k] == "" {
				t.Errorf("%s lacks --accent-%s", name, k)
			}
		}
		if t.Failed() {
			return
		}
		inv := hexLum(t, v["--accent-inverse"])
		check := func(what string, fg, bg float64) {
			if r := ratio(fg, bg); r < 4.5 {
				t.Errorf("%s %s = %.2f, want >= 4.5", name, what, r)
			}
		}
		check("button text", inv, hexLum(t, v["--accent-background"]))
		check("button text on light hover", inv, hexLum(t, v["--accent-light-hover-background"]))
		check("button text on dark hover", inv, hexLum(t, v["--accent-dark-hover-background"]))
		for _, bg := range []float64{lightPage, lightFoot} {
			check("light link", hexLum(t, v["--accent-light-primary"]), bg)
			check("light link hover", hexLum(t, v["--accent-light-hover"]), bg)
		}
		for _, bg := range []float64{darkPage, darkCard, darkFooter} {
			check("dark link", hexLum(t, v["--accent-dark-primary"]), bg)
			check("dark link hover", hexLum(t, v["--accent-dark-hover"]), bg)
		}
	}
}

func TestAccentMappingMirrorsPico(t *testing.T) {
	css := readStatic(t, "accents.css")
	light := strings.Index(css, `:root:not([data-theme="dark"])`)
	media := strings.Index(css, "@media (prefers-color-scheme: dark)")
	auto := strings.Index(css, ":root:not([data-theme])")
	forced := strings.Index(css, `:root[data-theme="dark"]`)
	if light < 0 || media < 0 || auto < media || forced < auto || light > media {
		t.Fatalf("mapping rules missing or out of order: %d %d %d %d", light, media, auto, forced)
	}
	block := func(at int) string {
		open := strings.Index(css[at:], "{")
		end := strings.Index(css[at+open:], "}")
		return css[at+open : at+open+end]
	}
	for _, c := range []struct {
		at  int
		set string
	}{{light, "light"}, {auto, "dark"}, {forced, "dark"}} {
		b := block(c.at)
		for pico, acc := range map[string]string{
			"--pico-text-selection-color":     "--accent-" + c.set + "-selection",
			"--pico-primary":                  "--accent-" + c.set + "-primary",
			"--pico-primary-background":       "--accent-background",
			"--pico-primary-underline":        "--accent-" + c.set + "-underline",
			"--pico-primary-hover":            "--accent-" + c.set + "-hover",
			"--pico-primary-hover-background": "--accent-" + c.set + "-hover-background",
			"--pico-primary-focus":            "--accent-" + c.set + "-focus",
			"--pico-primary-inverse":          "--accent-inverse",
			"--pico-switch-thumb-box-shadow":  "--accent-switch-shadow",
		} {
			if !strings.Contains(b, pico+": var("+acc+")") {
				t.Errorf("%s rule does not map %s to %s", c.set, pico, acc)
			}
		}
	}
}

// Jade is Pico's own accent: accents.css must agree with the embedded build, apart from the one
// dark hover shade that was darkened for AA.
func TestJadeAccentMatchesPicoBuild(t *testing.T) {
	jade := accentVars(t)["jade"]
	pico := readStatic(t, picoBuild)
	for _, want := range []string{"#007a50", "#005f3d", "#006d46", "#00b478", "#00cc88"} {
		if !strings.Contains(pico, want) {
			t.Fatalf("test assumption: pico build lacks %s", want)
		}
	}
	for k, want := range map[string]string{
		"--accent-background": "#007a50", "--accent-light-primary": "#007a50", "--accent-light-hover": "#005f3d",
		"--accent-light-hover-background": "#006d46", "--accent-dark-primary": "#00b478", "--accent-dark-hover": "#00cc88",
		"--accent-dark-hover-background": "#006d46",
	} {
		if jade[k] != want {
			t.Errorf("jade %s = %q, want %q", k, jade[k], want)
		}
	}
}

func TestAccentCSSLeavesBadgesAlone(t *testing.T) {
	css := readStatic(t, "accents.css")
	if strings.Contains(css, "--badge-") || strings.Contains(css, ".badge") {
		t.Error("badge colors are fixed and must not follow the accent")
	}
}
