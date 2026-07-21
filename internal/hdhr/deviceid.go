package hdhr

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
)

// HDHomeRun device IDs carry a checksum in their low nibble. Clients reject IDs
// that fail it, so an emulated device needs a synthetic ID that validates.
//
// The algorithm is from libhdhomerun: XOR the nibbles, passing every other one
// through a substitution table, and a valid ID reduces to zero. Verified
// against both real devices on this network (1234ABC2 and ABCDEF01 pass;
// single-nibble corruptions of each fail).
var deviceIDLookup = [16]uint32{
	0xA, 0x5, 0xF, 0x6, 0x7, 0xC, 0x1, 0xB,
	0x9, 0x2, 0x8, 0xD, 0x4, 0x3, 0xE, 0x0,
}

// deviceIDChecksum reduces id to zero when its low nibble is correct.
func deviceIDChecksum(id uint32) uint32 {
	var c uint32
	c ^= deviceIDLookup[(id>>28)&0x0F]
	c ^= (id >> 24) & 0x0F
	c ^= deviceIDLookup[(id>>20)&0x0F]
	c ^= (id >> 16) & 0x0F
	c ^= deviceIDLookup[(id>>12)&0x0F]
	c ^= (id >> 8) & 0x0F
	c ^= deviceIDLookup[(id>>4)&0x0F]
	c ^= (id >> 0) & 0x0F
	return c
}

// ValidDeviceID reports whether id is a well-formed HDHomeRun device ID.
//
// All-zeroes and all-ones satisfy the checksum but are reserved as the unset
// and wildcard values respectively, so both are rejected.
func ValidDeviceID(id uint32) bool {
	if id == 0x00000000 || id == 0xFFFFFFFF {
		return false
	}
	return deviceIDChecksum(id) == 0
}

// ParseDeviceID parses the eight-hex-digit form used on the wire.
func ParseDeviceID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("device id %q: not 8 hex digits: %w", s, err)
	}
	if !ValidDeviceID(uint32(n)) {
		return 0, fmt.Errorf("device id %q: fails checksum", s)
	}
	return uint32(n), nil
}

// FormatDeviceID renders an ID in the uppercase eight-digit form devices use.
func FormatDeviceID(id uint32) string { return fmt.Sprintf("%08X", id) }

// DeriveDeviceID produces a valid device ID from a set of stable inputs, such
// as the managed devices' addresses.
//
// Consumers key their tuner configuration off the device ID, so it must stay
// the same across restarts. Deriving it from the fleet rather than randomly
// guarantees that, while still giving distinct Sourcery instances distinct IDs.
func DeriveDeviceID(seeds []string) uint32 {
	sorted := append([]string(nil), seeds...)
	sort.Strings(sorted)

	h := fnv.New32a()
	for _, s := range sorted {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}

	id := fixChecksum(h.Sum32())

	// The reserved values must not escape. 0xFFFFFFF0 fixes up to the wildcard
	// 0xFFFFFFFF, so this is reachable, if barely.
	if !ValidDeviceID(id) {
		return fixChecksum(0x10000000)
	}
	return id
}

// fixChecksum sets id's low nibble so that the checksum validates.
func fixChecksum(id uint32) uint32 {
	id &^= 0x0F
	return id | deviceIDChecksum(id)
}
