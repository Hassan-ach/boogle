package util

import (
	"math"
	"os"
	"strconv"
)

func GetWithDefault(key, defaultValue string) string {
	k := os.Getenv(key)
	if k == "" {
		return defaultValue
	}
	return k
}

func GetIntWithDefault(key string, defaultValue int) int {
	k := GetWithDefault(key, "")
	v, err := strconv.Atoi(k)
	if err != nil {
		return defaultValue
	}
	return v
}

func GetFloatWithDefault(key string, defaultValue float64) float64 {
	k := GetWithDefault(key, "")
	v, err := strconv.ParseFloat(k, 64)
	if err != nil || math.IsNaN(v) {
		// NaN parses without error, but every comparison it takes part in is
		// false, so a weight of NaN would silently disable ranking instead of
		// failing loudly. Treat it as the misconfiguration it is.
		return defaultValue
	}
	return v
}
