package main

var masterRegions = [...]string{"asia", "europe", "america"}

// Newly issued identifiers retain UUID syntax and encode the fixed home in
// their random prefix. Initial identifiers use the immutable input index.
func idInRegion(region string) string {
	if !validRegion(region) {
		panic("invalid fixed user home")
	}
	for {
		value := id()
		if homeOfNewID(value) == region {
			return value
		}
	}
}

func homeOfNewID(value string) string {
	key, ok := uuidKey(value)
	if !ok {
		return ""
	}
	return masterRegions[int(key[0])%len(masterRegions)]
}

type masterHomes struct {
	users  map[string]string
	names  map[string]string
	images map[[16]byte]uint8
	posts  map[[16]byte]uint8
}

func (h *masterHomes) user(value string) string {
	if home := h.users[value]; home != "" {
		return home
	}
	return homeOfNewID(value)
}

func indexedHome(index map[[16]byte]uint8, value string) string {
	key, ok := uuidKey(value)
	if !ok {
		return ""
	}
	if code := index[key]; code != 0 {
		return masterRegions[int(code)-1]
	}
	return homeOfNewID(value)
}

func masterRegionCode(region string) uint8 {
	for i, name := range masterRegions {
		if name == region {
			return uint8(i + 1)
		}
	}
	return 0
}
