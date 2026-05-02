package control

import (
	"os"
)

func statSocket(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}
