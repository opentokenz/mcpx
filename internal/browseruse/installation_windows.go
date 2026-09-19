//go:build windows

package browseruse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type browserInstallRoot struct {
	Family string
	Path   string
}

func DiscoverInstalledOfficial() ([]Installation, error) {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		if home, err := os.UserHomeDir(); err == nil {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
	}
	if localAppData == "" {
		return nil, nil
	}
	roots := []browserInstallRoot{
		{Family: "edge", Path: filepath.Join(localAppData, "Microsoft", "Edge", "User Data")},
		{Family: "chrome", Path: filepath.Join(localAppData, "Google", "Chrome", "User Data")},
		{Family: "brave", Path: filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "User Data")},
		{Family: "chromium", Path: filepath.Join(localAppData, "Chromium", "User Data")},
	}
	return discoverInstalledOfficialFromRoots(roots), nil
}

func discoverInstalledOfficialFromRoots(roots []browserInstallRoot) []Installation {
	found := map[string]Installation{}
	for _, root := range roots {
		profiles, err := os.ReadDir(root.Path)
		if err != nil {
			continue
		}
		for _, profile := range profiles {
			if !profile.IsDir() {
				continue
			}
			extensionsDir := filepath.Join(root.Path, profile.Name(), "Extensions")
			extensions, err := os.ReadDir(extensionsDir)
			if err != nil {
				continue
			}
			for _, extension := range extensions {
				if !extension.IsDir() {
					continue
				}
				versionsDir := filepath.Join(extensionsDir, extension.Name())
				versions, err := os.ReadDir(versionsDir)
				if err != nil {
					continue
				}
				for _, version := range versions {
					if !version.IsDir() {
						continue
					}
					manifestPath := filepath.Join(versionsDir, version.Name(), "manifest.json")
					payload, err := os.ReadFile(manifestPath)
					if err != nil {
						continue
					}
					var manifest extensionManifest
					if json.Unmarshal(payload, &manifest) != nil || !isOfficialBrowserManifest(manifest) {
						continue
					}
					key := strings.ToLower(root.Family) + "\x00" + strings.ToLower(profile.Name()) + "\x00" + strings.ToLower(extension.Name())
					candidate := Installation{
						Family:       root.Family,
						Profile:      profile.Name(),
						ExtensionID:  extension.Name(),
						Name:         manifest.Name,
						Version:      manifest.Version,
						ManifestPath: manifestPath,
					}
					if current, ok := found[key]; !ok || candidate.Version > current.Version {
						found[key] = candidate
					}
				}
			}
		}
	}
	result := make([]Installation, 0, len(found))
	for _, installation := range found {
		result = append(result, installation)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Family != result[j].Family {
			return result[i].Family < result[j].Family
		}
		if result[i].Profile != result[j].Profile {
			return result[i].Profile < result[j].Profile
		}
		return result[i].ExtensionID < result[j].ExtensionID
	})
	return result
}
