package profiles

import _ "embed"

//go:embed ix1300-300dpi-duplex.json
var ix1300 []byte

// IX1300 returns the hardware-verified 300 dpi colour duplex profile.
func IX1300() []byte { return append([]byte(nil), ix1300...) }
