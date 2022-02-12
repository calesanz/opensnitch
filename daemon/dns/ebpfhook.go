package dns

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"

	"github.com/evilsocket/opensnitch/daemon/log"
	bpf "github.com/iovisor/gobpf/elf"
)

/*
#cgo LDFLAGS: -ldl

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <link.h>
#include <dlfcn.h>
#include <string.h>

char* find_libc() {
    void *handle;
    struct link_map * map;

    handle = dlopen(NULL, RTLD_NOW);
    if (handle == NULL) {
        fprintf(stderr, "EBPF-DNS dlopen() failed: %s\n", dlerror());
        return NULL;
    }


    if (dlinfo(handle, RTLD_DI_LINKMAP, &map) == -1) {
        fprintf(stderr, "EBPF-DNS: dlinfo failed: %s\n", dlerror());
        return NULL;
    }

    while(1){
        if(map == NULL){
            break;
        }

        if(strstr(map->l_name, "libc.so")){
            fprintf(stderr,"found %s\n", map->l_name);
            return map->l_name;
        }
        map = map->l_next;
    }
    return NULL;
}


*/
import "C"

type nameLookupEvent struct {
	AddrType uint32
	Ip       [16]uint8
	Host     [252]byte
}

func findLibc() (string, error) {
	ret := C.find_libc()

	if ret == nil {
		return "", errors.New("Could not find path to libc.so")
	}
	str := C.GoString(ret)

	return str, nil
}

// Iterates over all symbols in an elf file and returns the offset matching the provided symbol name.
func lookupSymbol(elffile *elf.File, symbolName string) (uint64, error) {
	symbols, err := elffile.Symbols()
	if err != nil {
		return 0, err
	}
	for _, symb := range symbols {
		if symb.Name == symbolName {
			return symb.Value, nil
		}
	}
	return 0, errors.New(fmt.Sprintf("Symbol: '%s' not found.", symbolName))
}

func DnsListenerEbpf() error {

	m := bpf.NewModule("/etc/opensnitchd/opensnitch-dns.o")
	if err := m.Load(nil); err != nil {
		log.Error("EBPF-DNS: Failed to load /etc/opensnitchd/opensnitch-dns.o: %v", err)
		return err
	}
	defer m.Close()

	// libbcc resolves the offsets for us. without bcc the offset for uprobes must parsed from the elf files
	// some how 0 must be replaced with the offset of getaddrinfo bcc does this using bcc_resolve_symname

	// Attaching to uprobe using perf open might be a better aproach requires https://github.com/iovisor/gobpf/pull/277
	libcFile, err := findLibc()

	if err != nil {
		log.Error("EBPF-DNS: Failed to find libc.so: %v", err)
		return err
	}

	libc_elf, err := elf.Open(libcFile)
	if err != nil {
		log.Error("EBPF-DNS: Failed to open %s: %v", libcFile, err)
		return err
	}
	probes_attached := 0
	for uprobe := range m.IterUprobes() {
		probeFunction := strings.Replace(uprobe.Name, "uretprobe/", "", 1)
		probeFunction = strings.Replace(probeFunction, "uprobe/", "", 1)
		offset, err := lookupSymbol(libc_elf, probeFunction)
		if err != nil {
			log.Warning("EBPF-DNS: Failed to find symbol for uprobe %s : %s\n", uprobe.Name, err)
			continue
		}
		err = bpf.AttachUprobe(uprobe, libcFile, offset)
		if err != nil {
			log.Error("EBPF-DNS: Failed to attach uprobe %s : %s\n", uprobe.Name, err)
			return err
		}
		probes_attached++
	}

	if probes_attached == 0 {
		log.Warning("EBPF-DNS: Failed to find symbols for uprobes.")
		return errors.New("Failed to find symbols for uprobes.")
	}

	// Reading Events
	channel := make(chan []byte)
	//log.Warning("EBPF-DNS: %+v\n", m)
	perfMap, err := bpf.InitPerfMap(m, "events", channel, nil)
	if err != nil {
		log.Error("EBPF-DNS: Failed to init perf map: %s\n", err)
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	go func() {
		var event nameLookupEvent
		for {
			data := <-channel
			log.Debug("EBPF-DNS: LookupEvent %d %x %x %x", len(data), data[:4], data[4:20], data[20:])
			err := binary.Read(bytes.NewBuffer(data), binary.LittleEndian, &event)
			if err != nil {
				log.Warning("EBPF-DNS: Failed to decode ebpf nameLookupEvent: %s\n", err)
				continue
			}
			// Convert C string (null-terminated) to Go string
			host := string(event.Host[:bytes.IndexByte(event.Host[:], 0)])
			var ip net.IP
			// 2 -> AF_INET (ipv4)
			if event.AddrType == 2 {
				ip = net.IP(event.Ip[:4])
			} else {
				ip = net.IP(event.Ip[:])
			}

			log.Debug("EBPF-DNS: Tracking Resolved Message: %s -> %s\n", host, ip.String())
			Track(ip.String(), host)
		}
	}()

	perfMap.PollStart()
	<-sig
	log.Info("EBPF-DNS: Received signal: terminating ebpf dns hook.")
	perfMap.PollStop()
	return nil
}
