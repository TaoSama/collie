package collie

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Getting a marketplace package's files onto the hub: either a directory of
// the repository at the pinned commit (path), or a built archive attached to
// one of its GitHub releases (package), checked against the sha256 and size
// the index gives. Both unpack the same careful way: no links, nothing
// outside the package directory, nothing larger than the limits.

const marketReleaseScheme = "release:" // package.url: release:<tag>/<file>

// MarketPackage is a built archive (.tar.gz, files at its root) attached to
// a release of the marketplace repository.
type MarketPackage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// release splits url into the release's tag and the asset's file name.
func (p *MarketPackage) release() (tag, file string, ok bool) {
	rest, ok := strings.CutPrefix(p.URL, marketReleaseScheme)
	if !ok {
		return "", "", false
	}
	tag, file, ok = strings.Cut(rest, "/")
	if !ok || tag == "" || file == "" || strings.ContainsAny(tag+file, "/?#% ") || strings.Contains(tag, "..") || file == "." || file == ".." {
		return "", "", false
	}
	return tag, file, true
}

func (p *MarketPackage) valid() bool {
	_, _, ok := p.release()
	_, err := hex.DecodeString(p.SHA256)
	return ok && err == nil && len(p.SHA256) == 64 && p.Size > 0 && p.Size <= marketTarMost
}

// validEntry: an id, a version, and exactly one of path and package.
func validEntry(e MarketEntry) bool {
	if !extIDRe.MatchString(e.ID) || e.Version == "" {
		return false
	}
	if e.Package != nil {
		return e.Path == "" && e.Package.valid()
	}
	return cleanRel(e.Path)
}

// fetchEntry writes the files of e into dst.
func (h *extHost) fetchEntry(ctx context.Context, repo, commit string, e *MarketEntry, dst string) error {
	if e.Package == nil {
		tarball, err := h.github(ctx, repo, "/tarball/"+commit, "application/vnd.github+json")
		if err != nil {
			return fmt.Errorf("下载：%v", err)
		}
		return extractSubdir(tarball, e.Path, dst)
	}
	b, err := h.downloadRelease(ctx, repo, e.Package)
	if err != nil {
		return fmt.Errorf("下载 %s：%v", e.Package.URL, err)
	}
	return extractTar(b, dst, marketTarMost, func(name string) (string, bool) {
		rel := strings.TrimPrefix(name, "./")
		return rel, rel != "" && rel != "."
	})
}

// downloadRelease fetches a release asset through the API (the repository
// token reads a private one) and checks its size and sha256.
func (h *extHost) downloadRelease(ctx context.Context, repo string, p *MarketPackage) ([]byte, error) {
	tag, file, ok := p.release()
	if !ok || !p.valid() {
		return nil, fmt.Errorf("索引里的 package 不对")
	}
	raw, err := h.github(ctx, repo, "/releases/tags/"+tag, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("读不到 Release %s：%v", tag, err)
	}
	var rel struct {
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, fmt.Errorf("Release %s 的应答不是合法的 JSON", tag)
	}
	var id int64
	for _, a := range rel.Assets {
		if a.Name == file {
			if a.Size != p.Size {
				return nil, fmt.Errorf("%s 有 %d 字节，索引写的是 %d", file, a.Size, p.Size)
			}
			id = a.ID
		}
	}
	if id == 0 {
		return nil, fmt.Errorf("Release %s 里没有 %s", tag, file)
	}
	b, err := h.githubLimit(ctx, repo, fmt.Sprintf("/releases/assets/%d", id), "application/octet-stream", p.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != p.Size {
		return nil, fmt.Errorf("下载到 %d 字节，索引写的是 %d", len(b), p.Size)
	}
	if sha256Hex(b) != strings.ToLower(p.SHA256) {
		return nil, fmt.Errorf("sha256 和索引不一致")
	}
	return b, nil
}

// extractSubdir writes the files under subdir of a GitHub tarball into dst,
// dropping its top directory.
func extractSubdir(tarball []byte, subdir, dst string) error {
	prefix := strings.Trim(subdir, "/") + "/"
	err := extractTar(tarball, dst, marketTarMost, func(name string) (string, bool) {
		_, name, ok := strings.Cut(name, "/")
		if !ok || !strings.HasPrefix(name, prefix) {
			return "", false
		}
		rel := strings.TrimPrefix(name, prefix)
		return rel, rel != ""
	})
	if err == errEmptyPackage {
		return fmt.Errorf("仓库里没有 %s", subdir)
	}
	return err
}

var errEmptyPackage = fmt.Errorf("包里没有文件")

// extractTar writes the entries of a .tar.gz that pick keeps (and renames)
// into dst. Links, devices, paths leaving dst, a file over extFileMost and
// more than most bytes in all are refused.
func extractTar(tgz []byte, dst string, most int64, pick func(name string) (string, bool)) error {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return fmt.Errorf("不是 gzip：%v", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	found := false
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hd.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, ok := pick(hd.Name)
		if !ok {
			continue
		}
		if !cleanRel(strings.TrimSuffix(rel, "/")) {
			return fmt.Errorf("包里有越界的路径 %q", rel)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			found = true
			if hd.Size > extFileMost {
				return fmt.Errorf("%s 超过 %d MB", rel, extFileMost>>20)
			}
			if total += hd.Size; total > most {
				return fmt.Errorf("包解开超过 %d MB", most>>20)
			}
			if err := writeTarFile(tr, target, hd); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("包里有链接 %q：不接受", rel)
		default:
			return fmt.Errorf("包里的 %q 不是普通文件或目录", rel)
		}
	}
	if !found {
		return errEmptyPackage
	}
	return nil
}

func writeTarFile(tr io.Reader, target string, hd *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if hd.Mode&0o111 != 0 {
		mode = 0o755
	}
	// O_EXCL: a name twice in one archive is refused, not overwritten.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if os.IsExist(err) {
		return fmt.Errorf("包里 %q 出现了两次", hd.Name)
	}
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(tr, hd.Size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != hd.Size {
		err = fmt.Errorf("%s 不完整", target)
	}
	return err
}
