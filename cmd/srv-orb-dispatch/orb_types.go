package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	srvconfig "srv/internal/config"
)

type orbResources struct {
	CPUs      int64 `json:"cpus"`
	MemoryMiB int64 `json:"memory_mib"`
}

// Resolve only at allocation: resuming an existing orb must not depend on a
// mapping file that may have changed or been removed since it was created.
func allocationResources(c config, orbType string) (string, string, error) {
	if orbType == "" {
		return c.cpus, c.ram, nil
	}
	if !filepath.IsAbs(c.typesFile) {
		return "", "", errors.New("ORB_TYPE requires an absolute SRV_ORB_TYPES_FILE")
	}
	data, err := os.ReadFile(c.typesFile)
	if err != nil {
		return "", "", fmt.Errorf("read SRV_ORB_TYPES_FILE: %w", err)
	}
	var types map[string]orbResources
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&types); err != nil {
		return "", "", fmt.Errorf("decode SRV_ORB_TYPES_FILE: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", "", errors.New("SRV_ORB_TYPES_FILE must contain one JSON object")
	}
	if len(types) == 0 {
		return "", "", errors.New("SRV_ORB_TYPES_FILE must contain at least one orb type")
	}
	for name, resources := range types {
		if strings.TrimSpace(name) == "" {
			return "", "", errors.New("SRV_ORB_TYPES_FILE contains an empty orb type name")
		}
		if err := srvconfig.ValidateMachineShape(resources.CPUs, resources.MemoryMiB); err != nil {
			return "", "", fmt.Errorf("orb type %q: %w", name, err)
		}
	}
	resources, ok := types[orbType]
	if !ok {
		return "", "", fmt.Errorf("unknown ORB_TYPE %q in SRV_ORB_TYPES_FILE", orbType)
	}
	return strconv.FormatInt(resources.CPUs, 10), strconv.FormatInt(resources.MemoryMiB, 10), nil
}
