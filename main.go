package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"text/template"

	"github.com/libvirt/libvirt-go"
)

// RequestData defines the JSON structure for VM creation
type RequestData struct {
	Name       string `json:"name"`
	ISOImage   string `json:"iso_image"`
	MemoryMB   int    `json:"memory_mb"`
	CPUs       int    `json:"cpus"`
	DiskSizeGB int    `json:"disk_size_gb"`
}

// ResponseData for success or error
type ResponseData struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// DomainXMLTemplate is a basic XML template for KVM/libvirt domain
// NOTE: This is a minimal example to create a VM with:
//   - One disk (qcow2) as vda
//   - One CD-ROM from the specified ISO
//   - A single NIC bound to "host-only-net"
const DomainXMLTemplate = `
<domain type='kvm'>
  <name>{{ .Name }}</name>
  <memory unit='MiB'>{{ .MemoryMB }}</memory>
  <currentMemory unit='MiB'>{{ .MemoryMB }}</currentMemory>
  <vcpu placement='static'>{{ .CPUs }}</vcpu>
  <os>
    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='cdrom'/>
  </os>
  <devices>
    <!-- CD-ROM device with provided ISO -->
    <disk type='file' device='cdrom'>
      <source file='{{ .ISOImage }}'/>
      <target dev='hdc' bus='ide'/>
      <readonly/>
    </disk>
    <!-- Primary disk using qcow2 -->
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/{{ .Name }}.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='network'>
      <source network='host-only-net'/>
      <model type='virtio'/>
    </interface>
    <graphics type='vnc' port='-1' listen='127.0.0.1'/>
  </devices>
</domain>
`

func main() {
	http.HandleFunc("/api/v1/vm", handleCreateVM)
	log.Println("ramanuj-vm-service listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// handleCreateVM handles the POST /api/v1/vm endpoint
func handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST is allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the incoming JSON request
	var req RequestData
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		log.Printf("Error decoding request: %v", err)
		http.Error(w, "Invalid JSON input", http.StatusBadRequest)
		return
	}

	// Validate minimal fields
	if req.Name == "" || req.ISOImage == "" || req.MemoryMB <= 0 || req.CPUs <= 0 || req.DiskSizeGB <= 0 {
		log.Printf("Invalid request parameters: %+v", req)
		http.Error(w, "Missing or invalid parameters", http.StatusBadRequest)
		return
	}

	// STEP 1: Create the qcow2 disk image
	diskPath := fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", req.Name)
	diskSizeArg := fmt.Sprintf("%dG", req.DiskSizeGB)

	// Example command: qemu-img create -f qcow2 /var/lib/libvirt/images/<VM_NAME>.qcow2 10G
	createDiskCmd := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, diskSizeArg)

	if output, err := createDiskCmd.CombinedOutput(); err != nil {
		errMsg := fmt.Sprintf("Failed to create disk image: %v, output: %s", err, string(output))
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}
	log.Printf("Disk created: %s of size %s", diskPath, diskSizeArg)

	// STEP 2: Connect to libvirt
	conn, err := libvirt.NewConnect("qemu:///system") // or "qemu+tcp://..."
	if err != nil {
		errMsg := fmt.Sprintf("Failed to connect to libvirt: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}
	defer func(conn *libvirt.Connect) {
		_, err := conn.Close()
		if err != nil {

		}
	}(conn)

	// STEP 3: Generate the domain XML from the template
	domainXML, err := generateDomainXML(req)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to generate domain XML: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}

	log.Printf("Generated domain XML for %s:\n%s\n", req.Name, domainXML)

	// STEP 4: Define the domain
	dom, err := conn.DomainDefineXML(domainXML)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to define domain: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}
	defer func(dom *libvirt.Domain) {
		err := dom.Free()
		if err != nil {

		}
	}(dom)

	// STEP 5: Start (create) the domain
	if err := dom.Create(); err != nil {
		// If we fail to start, attempt to undefine the domain to clean up
		_ = dom.Undefine()
		errMsg := fmt.Sprintf("Failed to start domain: %v", err)
		log.Println(errMsg)
		writeErrorResponse(w, errMsg)
		return
	}

	// If successful, respond with success
	writeSuccessResponse(w, "VM created and started successfully")
}

// generateDomainXML renders the domain XML template using the request data
func generateDomainXML(data RequestData) (string, error) {
	tpl, err := template.New("domainXML").Parse(DomainXMLTemplate)
	if err != nil {
		return "", err
	}

	var outStr string
	// We’ll render into a buffer
	err = tpl.Execute(newBuffer(&outStr), data)
	if err != nil {
		return "", err
	}
	return outStr, nil
}

// newBuffer is a small helper to allow template to write into a string.
type stringBuffer struct {
	str *string
}

func newBuffer(s *string) *stringBuffer {
	return &stringBuffer{
		str: s,
	}
}
func (sb *stringBuffer) Write(p []byte) (n int, err error) {
	*sb.str += string(p)
	return len(p), nil
}

// writeSuccessResponse writes a JSON success response
func writeSuccessResponse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	resp := ResponseData{
		Status:  "success",
		Message: msg,
	}
	json.NewEncoder(w).Encode(resp)
}

// writeErrorResponse writes a JSON error response
func writeErrorResponse(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusInternalServerError)
	w.Header().Set("Content-Type", "application/json")
	resp := ResponseData{
		Status:  "error",
		Message: msg,
	}
	json.NewEncoder(w).Encode(resp)
}
