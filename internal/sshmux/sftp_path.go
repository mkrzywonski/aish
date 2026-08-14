package sshmux

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

type sftpPath struct {
	Server string
	Native string
}

// normalizeSFTPPath is the single boundary between tool-facing target-native
// paths and the slash-drive form used by Windows OpenSSH's SFTP server.
func normalizeSFTPPath(style, input string) (sftpPath, error) {
	if input == "" {
		return sftpPath{}, errors.New("path must not be empty")
	}
	if strings.IndexByte(input, 0) >= 0 {
		return sftpPath{}, errors.New("path must not contain NUL")
	}

	switch style {
	case "posix":
		return normalizeSFTPPosixPath(input)
	case "windows":
		return normalizeSFTPWindowsPath(input)
	default:
		return sftpPath{}, errors.New("SFTP path style is unknown; no path translation is safe")
	}
}

func normalizeSFTPPosixPath(input string) (sftpPath, error) {
	if windowsDrivePath(input) || sftpPathStyle(input) == "windows" || strings.HasPrefix(input, `\\`) {
		return sftpPath{}, fmt.Errorf("path %q uses Windows syntax on a POSIX SFTP target", input)
	}
	if !strings.HasPrefix(input, "/") {
		return sftpPath{}, fmt.Errorf("path %q must be an absolute POSIX path", input)
	}
	clean := path.Clean(input)
	return sftpPath{Server: clean, Native: clean}, nil
}

func normalizeSFTPWindowsPath(input string) (sftpPath, error) {
	if strings.HasPrefix(input, `\\`) || strings.HasPrefix(input, "//") {
		return sftpPath{}, fmt.Errorf("UNC path %q is not supported by the observed drive-based SFTP namespace", input)
	}

	slash := strings.ReplaceAll(input, `\`, "/")
	if len(slash) >= 4 && slash[0] == '/' && driveLetter(slash[1]) && slash[2] == ':' && slash[3] == '/' {
		slash = slash[1:]
	}
	if !windowsDrivePath(slash) {
		return sftpPath{}, fmt.Errorf("path %q must be drive-absolute on this Windows SFTP target (for example C:\\Users\\name)", input)
	}

	drive := slash[0]
	if drive >= 'a' && drive <= 'z' {
		drive -= 'a' - 'A'
	}
	serverRoot := "/" + string(drive) + ":/"
	clean := path.Clean(serverRoot + slash[3:])
	if clean == "/"+string(drive)+":" {
		clean = serverRoot
	}
	if clean != serverRoot && !strings.HasPrefix(clean, serverRoot) {
		return sftpPath{}, fmt.Errorf("path %q escapes the %c: drive root", input, drive)
	}

	native := string(drive) + `:\`
	if clean != serverRoot {
		native += strings.ReplaceAll(strings.TrimPrefix(clean, serverRoot), "/", `\`)
	}
	return sftpPath{Server: clean, Native: native}, nil
}

func windowsDrivePath(p string) bool {
	return len(p) >= 3 && driveLetter(p[0]) && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}

func driveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
