// Package convert turns the signed raw disk into download formats:
// img.xz (USB writers), qcow2 (KVM/Proxmox), vmdk (streamOptimized),
// ova (VMware/VirtualBox appliance with EFI firmware) and iso (hybrid
// installer medium).
package convert

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/imgutil"
)

// Formats supported by the converter (raw.zst comes from finalize).
var Formats = []string{"img.xz", "qcow2", "vmdk", "ova", "iso"}

// Valid reports whether f is a known output format.
func Valid(f string) bool {
	for _, x := range append(Formats, "raw.zst") {
		if x == f {
			return true
		}
	}
	return false
}

// Default picks formats from the hardware targets.
func Default(virtual, physical bool) []string {
	var out []string
	if physical || !virtual {
		out = append(out, "img.xz")
	}
	if virtual {
		out = append(out, "qcow2")
	}
	return out
}

// Options for conversion.
type Options struct {
	QEMUImg     string
	Compression imgutil.Compression
	Name        string // appliance name for OVF
	MemoryMiB   int
	CPUs        int
	Epoch       time.Time // file timestamps (reproducible ISO)
}

// FormatError reports formats that could not be produced; the others are
// still returned by Run.
type FormatError map[string]error

func (e FormatError) Error() string {
	var parts []string
	for f, err := range e {
		parts = append(parts, f+": "+err.Error())
	}
	return "convert: " + strings.Join(parts, "; ")
}

// Run produces the requested formats from disk into dir; returns format →
// path. A failing format does not stop the others (FormatError lists them).
func Run(ctx context.Context, o Options, disk, dir string, formats []string) (map[string]string, error) {
	out := map[string]string{}
	fe := FormatError{}
	for _, f := range formats {
		if f == "vmdk" && contains(formats, "ova") {
			continue // produced together with ova
		}
		got, err := runOne(ctx, o, disk, dir, f, contains(formats, "vmdk"))
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			fe[f] = err
			continue
		}
		for k, v := range got {
			out[k] = v
		}
	}
	if len(fe) > 0 {
		return out, fe
	}
	return out, nil
}

func runOne(ctx context.Context, o Options, disk, dir, format string, keepVMDK bool) (map[string]string, error) {
	formats := []string{format}
	if format == "ova" && keepVMDK {
		formats = append(formats, "vmdk")
	}
	if o.QEMUImg == "" {
		o.QEMUImg = "qemu-img"
	}
	stem := filepath.Join(dir, strings.TrimSuffix(filepath.Base(disk), ".raw"))
	out := map[string]string{}
	need := map[string]bool{}
	for _, f := range formats {
		need[f] = true
	}
	if need["ova"] {
		need["vmdk"] = true
	}
	if need["img.xz"] {
		p := stem + ".img.xz"
		if err := imgutil.Xz(ctx, o.Compression, disk, p); err != nil {
			return nil, err
		}
		out["img.xz"] = p
	}
	if need["qcow2"] {
		p := stem + ".qcow2"
		if err := imgutil.Run(ctx, nil, o.QEMUImg, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "compat=1.1,lazy_refcounts=on", disk, p); err != nil {
			return nil, err
		}
		out["qcow2"] = p
	}
	if need["vmdk"] {
		p := stem + ".vmdk"
		if err := imgutil.Run(ctx, nil, o.QEMUImg, "convert", "-q", "-f", "raw", "-O", "vmdk", "-o", "subformat=streamOptimized,adapter_type=lsilogic", disk, p); err != nil {
			return nil, err
		}
		out["vmdk"] = p
	}
	if need["ova"] {
		st, err := os.Stat(disk)
		if err != nil {
			return nil, err
		}
		p := stem + ".ova"
		if err := writeOVA(p, out["vmdk"], o, st.Size()); err != nil {
			return nil, err
		}
		out["ova"] = p
		if !contains(formats, "vmdk") {
			os.Remove(out["vmdk"])
			delete(out, "vmdk")
		}
	}
	if need["iso"] {
		p := stem + ".iso"
		if err := ISO(ctx, disk, p, o.Name, o.Epoch); err != nil {
			return nil, err
		}
		out["iso"] = p
	}
	return out, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// OVF renders an OVF 1.0 descriptor for a single-disk EFI appliance.
func OVF(name, vmdkName string, vmdkSize, capacity int64, memMiB, cpus int) string {
	if memMiB == 0 {
		memMiB = 4096
	}
	if cpus == 0 {
		cpus = 2
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Envelope vmw:buildId="foundry" xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:cim="http://schemas.dmtf.org/wbem/wscim/1/common" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <References>
    <File ovf:href="%[2]s" ovf:id="file1" ovf:size="%[3]d"/>
  </References>
  <DiskSection>
    <Info>Virtual disk information</Info>
    <Disk ovf:capacity="%[4]d" ovf:capacityAllocationUnits="byte" ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
  </DiskSection>
  <NetworkSection>
    <Info>The list of logical networks</Info>
    <Network ovf:name="nat"><Description>NAT network</Description></Network>
  </NetworkSection>
  <VirtualSystem ovf:id="%[1]s">
    <Info>A Foundry-built operating system</Info>
    <Name>%[1]s</Name>
    <OperatingSystemSection ovf:id="96" vmw:osType="other5xLinux64Guest">
      <Info>Linux 64-bit</Info>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>%[1]s</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-14</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
        <rasd:Description>Number of Virtual CPUs</rasd:Description>
        <rasd:ElementName>%[6]d virtual CPU(s)</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>%[6]d</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:Description>Memory Size</rasd:Description>
        <rasd:ElementName>%[5]dMB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>%[5]d</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:Description>SCSI Controller</rasd:Description>
        <rasd:ElementName>SCSI Controller 0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceSubType>lsilogic</rasd:ResourceSubType>
        <rasd:ResourceType>6</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>0</rasd:AddressOnParent>
        <rasd:ElementName>Hard Disk 1</rasd:ElementName>
        <rasd:HostResource>ovf:/disk/vmdisk1</rasd:HostResource>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>17</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>7</rasd:AddressOnParent>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Connection>nat</rasd:Connection>
        <rasd:ElementName>Network adapter 1</rasd:ElementName>
        <rasd:InstanceID>5</rasd:InstanceID>
        <rasd:ResourceSubType>VmxNet3</rasd:ResourceSubType>
        <rasd:ResourceType>10</rasd:ResourceType>
      </Item>
      <vmw:Config ovf:required="false" vmw:key="firmware" vmw:value="efi"/>
      <vmw:Config ovf:required="false" vmw:key="uefi.secureBoot.enabled" vmw:value="false"/>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>
`, name, vmdkName, vmdkSize, capacity, memMiB, cpus)
}

func writeOVA(path, vmdk string, o Options, capacity int64) error {
	st, err := os.Stat(vmdk)
	if err != nil {
		return err
	}
	name := o.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".ova")
	}
	vmdkName := filepath.Base(vmdk)
	ovfName := strings.TrimSuffix(filepath.Base(path), ".ova") + ".ovf"
	ovf := []byte(OVF(name, vmdkName, st.Size(), capacity, o.MemoryMiB, o.CPUs))
	vsum, err := fileSHA256(vmdk)
	if err != nil {
		return err
	}
	osum := sha256.Sum256(ovf)
	mf := fmt.Sprintf("SHA256(%s)= %s\nSHA256(%s)= %s\n", ovfName, hex.EncodeToString(osum[:]), vmdkName, vsum)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	mt := time.Unix(0, 0)
	// OVF first, then manifest, then disks (OVA spec ordering).
	for _, e := range []struct {
		name string
		data []byte
	}{{ovfName, ovf}, {strings.TrimSuffix(ovfName, ".ovf") + ".mf", []byte(mf)}} {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), ModTime: mt, Format: tar.FormatUSTAR})
		tw.Write(e.data)
	}
	in, err := os.Open(vmdk)
	if err != nil {
		f.Close()
		return err
	}
	defer in.Close()
	tw.WriteHeader(&tar.Header{Name: vmdkName, Mode: 0o644, Size: st.Size(), ModTime: mt, Format: tar.FormatUSTAR})
	if _, err := io.Copy(tw, in); err != nil {
		f.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fileSHA256(p string) (string, error) {
	d, _, err := imgutil.SHA256File(p)
	return strings.TrimPrefix(d, "sha256:"), err
}
