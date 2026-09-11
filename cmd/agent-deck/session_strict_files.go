package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Open each component relative to a held parent directory; never follow a
// symlink in any ancestor or the leaf. Record directory device/inode authority
// so a second independent traversal detects replacement of an ancestor.
func strictOpenVerifiedPath(path string) (*os.File, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", fmt.Errorf("noncanonical path")
	}
	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	ancestors := []string{}
	for i, component := range components {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return nil, "", err
		}
		device, err := strconv.ParseUint(fmt.Sprint(st.Dev), 10, 64)
		if err != nil {
			_ = unix.Close(fd)
			return nil, "", fmt.Errorf("unsupported device identity")
		}
		ancestors = append(ancestors, fmt.Sprintf("%x:%d", device, st.Ino))
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, component, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, "", fmt.Errorf("nonregular native file")
	}
	return file, strings.Join(ancestors, "/"), nil
}

// strictOpenVerifiedDirectory is the directory-leaf counterpart to
// strictOpenVerifiedPath. Every component, including the leaf, is opened
// relative to a held parent with O_NOFOLLOW.
func strictOpenVerifiedDirectory(path string) (*os.File, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", fmt.Errorf("noncanonical directory path")
	}
	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	ancestors := []string{}
	for _, component := range components {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return nil, "", err
		}
		device, err := strconv.ParseUint(fmt.Sprint(st.Dev), 10, 64)
		if err != nil {
			_ = unix.Close(fd)
			return nil, "", fmt.Errorf("unsupported directory device identity")
		}
		ancestors = append(ancestors, fmt.Sprintf("%x:%d", device, st.Ino))
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		file.Close()
		return nil, "", fmt.Errorf("non-directory native path")
	}
	return file, strings.Join(ancestors, "/"), nil
}
func strictFileIdentity(file *os.File) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, 0, err
	}
	device, err := strconv.ParseUint(fmt.Sprint(stat.Dev), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unsupported device identity")
	}
	return device, stat.Ino, nil
}

// No buffering past the first record: message bytes are never read.
func strictReadFirstMeta(file *os.File) (strictSessionMeta, error) {
	var record strictSessionMeta
	data := make([]byte, 0, 2048)
	one := make([]byte, 1)
	for len(data) < 64*1024 {
		n, err := file.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				err := json.Unmarshal(data, &record)
				return record, err
			}
			data = append(data, one[0])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return record, fmt.Errorf("rollout read failed")
		}
	}
	return record, fmt.Errorf("missing bounded first session_meta record")
}
func strictReadSessionMeta(path string) (strictSessionMeta, error) {
	file, _, err := strictOpenVerifiedPath(path)
	if err != nil {
		return strictSessionMeta{}, err
	}
	defer file.Close()
	return strictReadFirstMeta(file)
}
