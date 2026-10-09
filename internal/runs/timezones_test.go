package runs

import "testing"

func TestEveryOfferedZoneIsValid(t *testing.T) {
	for _, g := range ZoneGroups("") {
		for _, z := range g.Zones {
			if !loadable(z) {
				t.Errorf("%s in region %s cannot be loaded", z, g.Region)
			}
		}
	}
}

func TestZoneGroupsKeepsASavedZoneOutsideTheList(t *testing.T) {
	has := func(groups []ZoneGroup, zone string) bool {
		for _, g := range groups {
			for _, z := range g.Zones {
				if z == zone {
					return true
				}
			}
		}
		return false
	}
	if has(ZoneGroups(""), "Asia/Ulaanbaatar") {
		t.Fatal("test zone should not be in the common list")
	}
	if !has(ZoneGroups("Asia/Ulaanbaatar"), "Asia/Ulaanbaatar") {
		t.Fatal("saved zone missing from the dropdown")
	}
	if has(ZoneGroups("Not/AZone"), "Not/AZone") {
		t.Fatal("invalid zone should not be offered")
	}
}
