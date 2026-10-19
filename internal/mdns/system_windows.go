package mdns

import (
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	dnsapi                     = windows.NewLazySystemDLL("dnsapi.dll")
	procDnsServiceBrowse       = dnsapi.NewProc("DnsServiceBrowse")
	procDnsServiceBrowseCancel = dnsapi.NewProc("DnsServiceBrowseCancel")

	// One callback serves every browse, since callbacks are never freed.
	browseCallback = windows.NewCallback(onBrowseResult)

	browsesMu sync.Mutex
	browses   = map[uintptr]*records{}
	browseID  uintptr
)

const (
	dnsQueryRequestVersion1 = 1
	dnsRequestPending       = 9506
	dnsFreeRecordList       = 1
)

// DNS_SERVICE_BROWSE_REQUEST
type browseRequest struct {
	version        uint32
	interfaceIndex uint32
	queryName      *uint16
	callback       uintptr
	context        uintptr
}

// DNS_SERVICE_CANCEL
type browseCancel struct{ reserved uintptr }

// systemBrowse asks the Windows DNS Client service, which current Windows
// 10 and 11 provide. It reports false if the service could not be asked.
func systemBrowse(ctx context.Context, fqdn string, wait time.Duration) (*records, bool) {
	if procDnsServiceBrowse.Find() != nil {
		return nil, false
	}
	recs := newRecords()
	browsesMu.Lock()
	browseID++
	id := browseID
	browses[id] = recs
	browsesMu.Unlock()

	name, _ := windows.UTF16PtrFromString(strings.TrimSuffix(fqdn, "."))
	req := &browseRequest{version: dnsQueryRequestVersion1, queryName: name, callback: browseCallback, context: id}
	cancel := &browseCancel{}
	r, _, _ := procDnsServiceBrowse.Call(uintptr(unsafe.Pointer(req)), uintptr(unsafe.Pointer(cancel)))
	if r == dnsRequestPending {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		procDnsServiceBrowseCancel.Call(uintptr(unsafe.Pointer(cancel)))
	}
	runtime.KeepAlive(req)

	// Callbacks hold browsesMu while adding, so after this recs is ours.
	browsesMu.Lock()
	delete(browses, id)
	browsesMu.Unlock()
	return recs, r == dnsRequestPending
}

func onBrowseResult(status uint32, id uintptr, rec *windows.DNSRecord) uintptr {
	if rec == nil {
		return 0
	}
	defer windows.DnsRecordListFree(rec, dnsFreeRecordList)
	browsesMu.Lock()
	defer browsesMu.Unlock()
	recs := browses[id]
	if recs == nil || status != 0 {
		return 0
	}
	for r := rec; r != nil; r = r.Next {
		owner := unescape(windows.UTF16PtrToString(r.Name))
		data := unsafe.Pointer(&r.Data[0])
		switch r.Type {
		case windows.DNS_TYPE_PTR:
			recs.addPTR(owner, unescape(windows.UTF16PtrToString((*windows.DNSPTRData)(data).Host)), nil)
		case windows.DNS_TYPE_SRV:
			d := (*windows.DNSSRVData)(data)
			recs.addSRV(owner, unescape(windows.UTF16PtrToString(d.Target)), int(d.Port))
		case windows.DNS_TYPE_TEXT:
			d := (*windows.DNSTXTData)(data)
			var txt []string
			for _, s := range unsafe.Slice(&d.StringArray[0], int(d.StringCount)) {
				txt = append(txt, windows.UTF16PtrToString(s))
			}
			recs.addTXT(owner, txt)
		case windows.DNS_TYPE_A:
			recs.addA(owner, net.IPv4(r.Data[0], r.Data[1], r.Data[2], r.Data[3]))
		}
	}
	return 0
}
