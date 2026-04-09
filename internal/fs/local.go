package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type TargetDecision struct {
	FinalPath   string
	DirToEnsure string
	IsDirectory bool
}

type LocalFile struct {
	Path string
	Rel  string
	Size int64
}

func ResolveDownloadTarget(input, cwd, defaultBase string, expectDirectory bool) (TargetDecision, error) {
	if strings.TrimSpace(defaultBase) == "" {
		return TargetDecision{}, fmt.Errorf("defaultBase 不能为空")
	}

	trimmedInput := strings.TrimSpace(input)
	if trimmedInput == "" {
		return TargetDecision{
			FinalPath:   filepath.Join(cwd, defaultBase),
			IsDirectory: expectDirectory,
		}, nil
	}

	cleaned := filepath.Clean(trimmedInput)
	if filepath.IsAbs(trimmedInput) {
		cleaned = filepath.Clean(trimmedInput)
	} else {
		cleaned = filepath.Clean(trimmedInput)
	}

	if info, err := os.Stat(cleaned); err == nil {
		if info.IsDir() {
			return TargetDecision{
				FinalPath:   filepath.Join(cleaned, defaultBase),
				IsDirectory: true,
			}, nil
		}
		if expectDirectory {
			return TargetDecision{}, fmt.Errorf("本地目标 %s 不是目录", cleaned)
		}
		return TargetDecision{
			FinalPath:   cleaned,
			DirToEnsure: filepath.Dir(cleaned),
			IsDirectory: false,
		}, nil
	}

	if expectDirectory || LooksLikeDirectory(trimmedInput) {
		return TargetDecision{
			FinalPath:   filepath.Join(cleaned, defaultBase),
			DirToEnsure: cleaned,
			IsDirectory: true,
		}, nil
	}

	return TargetDecision{
		FinalPath:   cleaned,
		DirToEnsure: filepath.Dir(cleaned),
		IsDirectory: false,
	}, nil
}

func ResolveDirectoryBase(input, cwd string) (TargetDecision, error) {
	trimmedInput := strings.TrimSpace(input)
	if trimmedInput == "" {
		return TargetDecision{
			FinalPath:   cwd,
			IsDirectory: true,
		}, nil
	}

	cleaned := filepath.Clean(trimmedInput)
	if info, err := os.Stat(cleaned); err == nil {
		if !info.IsDir() {
			return TargetDecision{}, fmt.Errorf("本地目标 %s 不是目录", cleaned)
		}
		return TargetDecision{
			FinalPath:   cleaned,
			IsDirectory: true,
		}, nil
	}

	return TargetDecision{
		FinalPath:   cleaned,
		DirToEnsure: cleaned,
		IsDirectory: true,
	}, nil
}

func LooksLikeDirectory(input string) bool {
	if input == "" {
		return false
	}
	if strings.HasSuffix(input, string(os.PathSeparator)) {
		return true
	}
	base := filepath.Base(input)
	return filepath.Ext(base) == ""
}

func AutoRename(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	dir := filepath.Dir(path)

	for index := 1; ; index++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s(%d)%s", base, index, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func CollectLocalFiles(root string) ([]LocalFile, int64, error) {
	root = filepath.Clean(root)
	total := int64(0)
	files := make([]LocalFile, 0)

	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}

		files = append(files, LocalFile{
			Path: current,
			Rel:  rel,
			Size: info.Size(),
		})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return files, total, nil
}

func HasGlob(path string) bool {
	return strings.ContainsAny(path, "*?[")
}
