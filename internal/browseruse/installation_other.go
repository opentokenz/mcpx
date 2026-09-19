//go:build !windows

package browseruse

func DiscoverInstalledOfficial() ([]Installation, error) {
	return nil, ErrUnsupported
}
