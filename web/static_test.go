package web

import (
	"regexp"
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
		t.Errorf("app.css overrides the accent (%q); choose the Pico build instead", m)
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
