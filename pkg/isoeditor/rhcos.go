package isoeditor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/openshift/assisted-image-service/internal/common"
	log "github.com/sirupsen/logrus"
)

const (
	RamDiskPaddingLength        = uint64(1024 * 1024) // 1MB
	NmstatectlPathInRamdisk     = "/usr/bin/nmstatectl"
	ramDiskImagePath            = "/images/assisted_installer_custom.img"
	nmstateDiskImagePath        = "/images/nmstate.img"
	MinimalVersionForNmstatectl = "4.18.0-ec.0"
)

//go:generate mockgen -package=isoeditor -destination=mock_editor.go . Editor
type Editor interface {
	CreateMinimalISOTemplate(fullISOPath, rootFSURL, arch, minimalISOPath, openshiftVersion, nmstatectlPath string) error
}

type rhcosEditor struct {
	workDir        string
	nmstateHandler NmstateHandler
}

func NewEditor(dataDir string, nmstateHandler NmstateHandler) Editor {
	return &rhcosEditor{
		workDir:        dataDir,
		nmstateHandler: nmstateHandler,
	}
}

// CreateMinimalISO Creates the minimal iso by removing the rootfs and adding the url
func CreateMinimalISO(extractDir, volumeID, rootFSURL, arch, minimalISOPath string) error {
	if err := os.Remove(filepath.Join(extractDir, "images/pxeboot/rootfs.img")); err != nil {
		return err
	}

	if err := embedInitrdPlaceholders(extractDir); err != nil {
		log.WithError(err).Warnf("Failed to embed initrd placeholders")
		return err
	}

	var includeNmstateRamDisk bool
	if _, err := os.Stat(filepath.Join(extractDir, nmstateDiskImagePath)); err == nil {
		includeNmstateRamDisk = true
	}

	if err := fixGrubConfig(rootFSURL, extractDir, includeNmstateRamDisk); err != nil {
		log.WithError(err).Warnf("Failed to edit grub config")
		return err
	}

	// ignore isolinux.cfg for ppc64le because it doesn't exist
	if arch != "ppc64le" {
		if err := fixIsolinuxConfig(rootFSURL, extractDir, includeNmstateRamDisk); err != nil {
			log.WithError(err).Warnf("Failed to edit isolinux config")
			return err
		}
	}

	if err := Create(minimalISOPath, extractDir, volumeID); err != nil {
		return err
	}
	return nil
}

// CreateMinimalISOTemplate Creates the template minimal iso by removing the rootfs and adding the url
func (e *rhcosEditor) CreateMinimalISOTemplate(fullISOPath, rootFSURL, arch, minimalISOPath, openshiftVersion, nmstatectlPath string) error {
	extractDir, err := os.MkdirTemp(e.workDir, "isoutil")
	if err != nil {
		return err
	}

	if err = Extract(fullISOPath, extractDir); err != nil {
		return err
	}

	volumeID, err := VolumeIdentifier(fullISOPath)
	if err != nil {
		return err
	}

	ramDiskPath := filepath.Join(extractDir, nmstateDiskImagePath)

	versionOK, err := common.VersionGreaterOrEqual(openshiftVersion, MinimalVersionForNmstatectl)
	if err != nil {
		return err
	}

	if versionOK {
		rootfsPath := filepath.Join(extractDir, "images/pxeboot/rootfs.img")
		err = e.nmstateHandler.CreateNmstateRamDisk(rootfsPath, ramDiskPath, nmstatectlPath)
		if err != nil {
			return fmt.Errorf("failed to create nmstate ram disk for arch %s: %v", arch, err)
		}
	}

	err = CreateMinimalISO(extractDir, volumeID, rootFSURL, arch, minimalISOPath)
	if err != nil {
		return err
	}

	return nil
}

func embedInitrdPlaceholders(extractDir string) error {
	f, err := os.Create(filepath.Join(extractDir, ramDiskImagePath))
	if err != nil {
		return err
	}
	defer func() {
		if deferErr := f.Sync(); deferErr != nil {
			log.WithError(deferErr).Error("Failed to sync disk image placeholder file")
		}
		if deferErr := f.Close(); deferErr != nil {
			log.WithError(deferErr).Error("Failed to close disk image placeholder file")
		}
	}()

	err = f.Truncate(int64(RamDiskPaddingLength))
	if err != nil {
		return err
	}

	return nil
}

func fixGrubConfig(rootFSURL, extractDir string, includeNmstateRamDisk bool) error {
	availableGrubPaths := []string{"EFI/redhat/grub.cfg", "EFI/fedora/grub.cfg", "boot/grub/grub.cfg", "EFI/centos/grub.cfg"}
	var foundGrubPath string
	for _, pathSection := range availableGrubPaths {
		path := filepath.Join(extractDir, pathSection)
		if _, err := os.Stat(path); err == nil {
			foundGrubPath = path
			break
		}
	}
	if len(foundGrubPath) == 0 {
		return fmt.Errorf("no grub.cfg found, possible paths are %v", availableGrubPaths)
	}

	// Add the rootfs url
	replacement := fmt.Sprintf("$1 $2 'coreos.live.rootfs_url=%s'", rootFSURL)
	if err := editFile(foundGrubPath, `(?m)^(\s+linux) (.+| )+$`, replacement); err != nil {
		return err
	}

	// Remove the coreos.liveiso parameter
	if err := editFile(foundGrubPath, ` coreos.liveiso=\S+`, ""); err != nil {
		return err
	}

	// Edit config to add custom ramdisk image to initrd
	if includeNmstateRamDisk {
		if err := editFile(foundGrubPath, `(?m)^(\s+initrd) (.+| )+$`, fmt.Sprintf("$1 $2 %s %s", ramDiskImagePath, nmstateDiskImagePath)); err != nil {
			return err
		}
	} else {
		if err := editFile(foundGrubPath, `(?m)^(\s+initrd) (.+| )+$`, fmt.Sprintf("$1 $2 %s", ramDiskImagePath)); err != nil {
			return err
		}
	}

	return nil
}

func fixIsolinuxConfig(rootFSURL, extractDir string, includeNmstateRamDisk bool) error {
	replacement := fmt.Sprintf("$1 $2 coreos.live.rootfs_url=%s", rootFSURL)
	if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), `(?m)^(\s+append) (.+| )+$`, replacement); err != nil {
		return err
	}

	if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), ` coreos.liveiso=\S+`, ""); err != nil {
		return err
	}

	if includeNmstateRamDisk {
		if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), `(?m)^(\s+append.*initrd=\S+) (.*)$`, fmt.Sprintf("${1},%s,%s ${2}", ramDiskImagePath, nmstateDiskImagePath)); err != nil {
			return err
		}
	} else {
		if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), `(?m)^(\s+append.*initrd=\S+) (.*)$`, fmt.Sprintf("${1},%s ${2}", ramDiskImagePath)); err != nil {
			return err
		}
	}

	return nil
}

func editFile(fileName string, reString string, replacement string) error {
	content, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}

	re := regexp.MustCompile(reString)
	newContent := re.ReplaceAllString(string(content), replacement)

	if err := os.WriteFile(fileName, []byte(newContent), 0600); err != nil {
		return err
	}

	return nil
}
