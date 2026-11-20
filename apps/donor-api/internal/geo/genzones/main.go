// Command genzones writes internal/geo/zones.json from the US ZIP three-digit
// prefix (SCF) ranges, mapping each to its IANA time zone. It is run by hand
// when the mapping changes (go generate ./internal/geo); the committed
// zones.json is the source of truth the geo package embeds. Keeping the derivation
// here, rather than hand-editing a 900-line JSON, is what makes the table auditable.
//
// Ranges are inclusive three-digit prefixes "000".."999". Where a prefix range
// crosses a zone line, the dominant zone is used and specific ZIPs are corrected
// in overrides below.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
)

// zoneRange assigns IANA zone to every three-digit prefix in [lo, hi].
type zoneRange struct {
	lo, hi int
	zone   string
}

const (
	eastern    = "America/New_York"
	central    = "America/Chicago"
	mountain   = "America/Denver"
	arizona    = "America/Phoenix" // Mountain without DST
	pacific    = "America/Los_Angeles"
	alaska     = "America/Anchorage"
	hawaii     = "Pacific/Honolulu"
	puertoRico = "America/Puerto_Rico" // Atlantic, no DST
	guam       = "Pacific/Guam"
)

// ranges covers the assigned US ZIP prefixes. Derived from USPS SCF prefix
// assignments by state and each state's prevailing time zone.
var ranges = []zoneRange{
	{5, 5, eastern},      // NY/CT remittance prefix (eastern)
	{6, 9, puertoRico},   // PR and USVI (Atlantic, no DST)
	{10, 149, eastern},   // NY, PA, DE, NJ, New England (010+ is MA/CT/etc.)
	{150, 199, eastern},  // PA
	{200, 219, eastern},  // DC, MD, VA
	{220, 246, eastern},  // VA, WV
	{247, 253, eastern},  // WV
	{254, 268, eastern},  // VA/NC/SC (Carolinas eastern)
	{270, 289, eastern},  // NC
	{290, 299, eastern},  // SC
	{300, 319, eastern},  // GA
	{320, 323, eastern},  // FL
	{324, 325, central},  // FL panhandle (Gulf County east of the canal corrected in overrides)
	{326, 349, eastern},  // FL
	{350, 369, central},  // AL
	{370, 372, central},  // TN (Nashville)
	{373, 374, eastern},  // TN (Chattanooga)
	{375, 375, central},  // TN (Memphis)
	{376, 379, eastern},  // TN (Tri-Cities, Knoxville)
	{380, 385, central},  // TN (Memphis and west)
	{386, 397, central},  // MS
	{398, 399, eastern},  // GA remit
	{400, 419, eastern},  // KY
	{420, 424, central},  // KY (Paducah, Owensboro, Bowling Green, Henderson)
	{425, 427, eastern},  // KY
	{430, 459, eastern},  // OH
	{460, 479, eastern},  // IN (most eastern; NW/SW corrected in overrides)
	{480, 499, eastern},  // MI (UP west corrected in overrides)
	{500, 528, central},  // IA
	{530, 549, central},  // WI
	{550, 567, central},  // MN
	{570, 576, central},  // SD
	{577, 577, mountain}, // SD (Rapid City)
	{580, 588, central},  // ND (west corrected in overrides)
	{590, 599, mountain}, // MT
	{600, 629, central},  // IL
	{630, 658, central},  // MO
	{660, 679, central},  // KS (west corrected in overrides)
	{680, 693, central},  // NE (west corrected in overrides)
	{700, 714, central},  // LA
	{716, 729, central},  // AR
	{730, 749, central},  // OK
	{750, 799, central},  // TX (El Paso area corrected in overrides)
	{800, 816, mountain}, // CO
	{820, 831, mountain}, // WY
	{832, 834, mountain}, // ID
	{835, 835, pacific},  // ID (Lewiston)
	{836, 837, mountain}, // ID (Boise)
	{838, 838, pacific},  // ID (north)
	{840, 847, mountain}, // UT
	{850, 865, arizona},  // AZ (no DST)
	{870, 884, mountain}, // NM
	{885, 885, mountain}, // TX (El Paso)
	{889, 898, pacific},  // NV
	{900, 961, pacific},  // CA
	{967, 968, hawaii},   // HI
	{969, 969, guam},     // Guam
	{970, 979, pacific},  // OR (east corrected in overrides)
	{980, 994, pacific},  // WA
	{995, 999, alaska},   // AK
}

// overrides correct individual five-digit ZIPs whose prefix region straddles a
// zone line. Representative anchors for each straddle region; the prefix default
// covers the majority and a donor can correct their own zone in-profile.
var overrides = map[string]string{
	// Gulf County, Florida (eastern), inside the central panhandle prefix 324.
	"32456": eastern, "32465": eastern,
	// Northwest Indiana (central), prefix 463/464 defaults eastern.
	"46301": central, "46307": central, "46320": central, "46321": central,
	"46322": central, "46327": central, "46342": central, "46360": central,
	"46402": central, "46403": central, "46404": central, "46406": central,
	"46408": central, "46409": central, "46410": central,
	// Michigan Upper Peninsula west (central), prefix 498/499 defaults eastern.
	"49801": central, "49802": central, "49807": central, "49815": central,
	"49821": central, "49829": central, "49834": central, "49847": central,
	"49858": central, "49866": central, "49870": central, "49874": central,
	// West North Dakota (mountain), prefix 586/587/588 defaults central.
	"58601": mountain, "58602": mountain, "58631": mountain, "58634": mountain,
	"58640": mountain, "58701": central, "58801": mountain, "58854": mountain,
	// West Kansas (mountain), prefix 677/679 defaults central.
	"67701": mountain, "67731": mountain, "67801": mountain, "67846": mountain,
	"67880": mountain, "67901": mountain, "67951": mountain,
	// West Nebraska (mountain), prefix 691/693 defaults central.
	"69101": mountain, "69120": mountain, "69130": mountain, "69301": mountain,
	"69341": mountain, "69357": mountain, "69361": mountain,
	// West Texas / El Paso (mountain), prefix 798/799 defaults central.
	"79821": mountain, "79835": mountain, "79901": mountain, "79902": mountain,
	"79903": mountain, "79904": mountain, "79905": mountain, "79906": mountain,
	"79907": mountain, "79912": mountain, "79915": mountain, "79922": mountain,
	"79924": mountain, "79925": mountain, "79930": mountain, "79936": mountain,
	// West Wendover, Nevada observes mountain time, prefix 898 defaults pacific.
	"89883": mountain,
	// East Oregon (mountain), prefix 978/979 defaults pacific.
	"97901": mountain, "97903": mountain, "97905": mountain, "97907": mountain,
	"97910": mountain, "97913": mountain, "97914": mountain, "97918": mountain,
}

func main() {
	out := flag.String("o", "zones.json", "file to write; the default is the embedded table when run by go generate in internal/geo")
	flag.Parse()
	buf, err := render()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n", *out)
}

// render returns the zones.json content. encoding/json sorts map keys, so the
// committed file has a deterministic diff.
func render() ([]byte, error) {
	prefix := map[string]string{}
	for _, r := range ranges {
		for p := r.lo; p <= r.hi; p++ {
			prefix[fmt.Sprintf("%03d", p)] = r.zone
		}
	}
	buf, err := json.MarshalIndent(struct {
		Prefix   map[string]string `json:"prefix"`
		Override map[string]string `json:"override"`
	}{Prefix: prefix, Override: overrides}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
}
