package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"text/template"
	"time"

	"github.com/google/uuid"
	libvirt "github.com/libvirt/libvirt-go"
)

var domainXMLTemplate *template.Template

// RequestData - incoming JSON to define how the VM should be created
type RequestData struct {
	Name             string `json:"name"`
	PrebuiltDiskPath string `json:"prebuilt_disk_path,omitempty"`
	ISOImage         string `json:"iso_image,omitempty"`
	MemoryMB         int    `json:"memory_mb"`
	CPUs             int    `json:"cpus"`
	DiskSizeGB       int    `json:"disk_size_gb"`
}

// DiskDevice - represents a disk in the final domain XML
type DiskDevice struct {
	Dev  string // e.g., "vda", "vdb"
	Path string // path to qcow2 on host
}

// TemplateData - all fields we inject into vm-template.xml
type TemplateData struct {
	Name       string
	UUID       string
	MemoryKiB  int
	CPUs       int
	MacAddress string

	// Disks: a slice for one (root) + optional second
	Disks []DiskDevice

	// If user specified an ISO, we attach a CDROM
	HasISO   bool
	ISOImage string
}

type ResponseData struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func init() {
	rand.Seed(time.Now().UnixNano())

	// Load the external XML template at startup
	content, err := os.ReadFile("vm-template.xml")
	if err != nil {
		log.Fatalf("Failed to read vm-template.xml: %v", err)
	}
	domainXMLTemplate, err = template.New("domainXML").Parse(string(content))
	if err != nil {
		log.Fatalf("Failed to parse vm-template.xml as template: %v", err)
	}
}

func main() {
	http.HandleFunc("/api/v1/vm", handleCreateVM)
	log.Println("padmini-vm-service listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

func handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST is allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RequestData
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Error decoding JSON: %v", err)
		http.Error(w, "Invalid JSON input", http.StatusBadRequest)
		return
	}

	// Basic validation
	if req.Name == "" || req.MemoryMB <= 0 || req.CPUs <= 0 {
		msg := fmt.Sprintf("Missing/invalid request fields: %+v", req)
		log.Println(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// STEP 1: Build our list of disk devices
	// (We might create or skip creation depending on user input)

	// We'll hold two potential disk devices: root (vda), optional data (vdb)
	var disks []DiskDevice

	rootDev := "vda"
	var rootDiskPath string

	if req.PrebuiltDiskPath != "" {
		// Use the existing qcow2 as the root disk
		rootDiskPath = req.PrebuiltDiskPath
		// Don't create a new file for the root disk
		log.Printf("User provided an existing disk for root: %s", rootDiskPath)
	} else {
		// If the user didn't provide a prebuilt disk, create one for root
		rootDiskPath = fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", req.Name)
		if err := createQcow2Disk(rootDiskPath, req.DiskSizeGB); err != nil {
			errMsg := fmt.Sprintf("Failed to create root disk: %v", err)
			log.Println(errMsg)
			writeErrorResponse(w, errMsg)
			return
		}
	}

	// Add root disk to the slice
	disks = append(disks, DiskDevice{
		Dev:  rootDev,
		Path: rootDiskPath,
	})

	// If user wants an additional disk (req.DiskSizeGB > 0) AND they used a prebuilt root,
	// we can create that new disk as "vdb". If they used a prebuilt root *and* gave a size,
	// we interpret that as "I want a second data disk."
	//
	// BUT if they provided a prebuilt disk and also "disk_size_gb", we have to decide:
	// do we treat the disk_size_gb as "root" or "data"?
	// We'll interpret it as an extra data disk (since the user root is from prebuilt).
	// If user is installing from ISO onto a new root, that also uses disk_size_gb above
	// (already created for vda). So let's handle a potential second disk carefully:

	// We'll do a simple rule:
	// - If PrebuiltDiskPath != "" and DiskSizeGB > 0 => create a second disk as vdb.
	// - If PrebuiltDiskPath == "" => we used DiskSizeGB for the root disk already (vda).
	//   The user can specify a separate "data_size_gb" field if we wanted a second disk
	//   but let's keep it simple for now. We'll assume they only do 1 disk in that scenario.

	// For a more thorough approach, you might define an array of volumes or something similar in the API.
	if req.PrebuiltDiskPath != "" && req.DiskSizeGB > 0 {
		dataDiskPath := fmt.Sprintf("/var/lib/libvirt/images/%s-data.qcow2", req.Name)
		if err := createQcow2Disk(dataDiskPath, req.DiskSizeGB); err != nil {
			errMsg := fmt.Sprintf("Failed to create additional data disk: %v", err)
			log.Println(errMsg)
			writeErrorResponse(w, errMsg)
			return
		}

		// Add second disk as vdb
		disks = append(disks, DiskDevice{
			Dev:  "vdb",
			Path: dataDiskPath,
		})
	}

	// STEP 2: Connect to libvirt
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		errMsg := fmt.Sprintf("Failed to connect libvirt: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}
	defer conn.Close()

	// STEP 3: Generate domain XML
	xmlContent, err := generateDomainXML(req, disks)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to generate domain XML: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}

	log.Printf("Domain XML:\n%s\n", xmlContent)

	// STEP 4: Define domain
	dom, err := conn.DomainDefineXML(xmlContent)
	if err != nil {
		errMsg := fmt.Sprintf("DomainDefineXML failed: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}
	defer dom.Free()

	// STEP 5: Start domain
	if err := dom.Create(); err != nil {
		_ = dom.Undefine()
		errMsg := fmt.Sprintf("Failed to start domain: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}

	writeSuccessResponse(w, "VM created and started successfully")
}

// createQcow2Disk is a helper to call qemu-img create
func createQcow2Disk(path string, sizeGB int) error {
	if sizeGB <= 0 {
		return fmt.Errorf("disk_size_gb must be > 0 to create a new disk")
	}
	sizeArg := fmt.Sprintf("%dG", sizeGB)
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", path, sizeArg)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create failed: %v, output: %s", err, string(output))
	}
	log.Printf("Created disk %s (%s)", path, sizeArg)
	return nil
}

// generateDomainXML populates the vm-template with the relevant fields
func generateDomainXML(req RequestData, disks []DiskDevice) (string, error) {
	data := TemplateData{
		Name:       req.Name,
		UUID:       uuid.New().String(),
		MemoryKiB:  req.MemoryMB * 1024,
		CPUs:       req.CPUs,
		MacAddress: generateRandomMAC(),

		Disks: disks,

		HasISO:   (req.ISOImage != ""),
		ISOImage: req.ISOImage,
	}

	var outStr string
	if err := domainXMLTemplate.Execute(newBuffer(&outStr), data); err != nil {
		return "", err
	}
	return outStr, nil
}

// Simple buffer to capture template output
type stringBuffer struct {
	str *string
}

func newBuffer(s *string) *stringBuffer {
	return &stringBuffer{str: s}
}

func (sb *stringBuffer) Write(p []byte) (n int, err error) {
	*sb.str += string(p)
	return len(p), nil
}

// Generate a random MAC address with QEMU-friendly prefix 52:54:00
func generateRandomMAC() string {
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x",
		rand.Intn(256),
		rand.Intn(256),
		rand.Intn(256),
	)
}

// writeSuccessResponse
func writeSuccessResponse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	resp := ResponseData{Status: "success", Message: msg}
	_ = json.NewEncoder(w).Encode(resp)
}

// writeErrorResponse
func writeErrorResponse(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusInternalServerError)
	w.Header().Set("Content-Type", "application/json")
	resp := ResponseData{Status: "error", Message: msg}
	_ = json.NewEncoder(w).Encode(resp)
}
