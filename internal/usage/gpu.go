// GPUs are the ones NVML sees, which Slurm limits to the job's. Each sample takes their metrics from DCGM when it
// watches every job GPU, and from NVML otherwise, so DCGM starting late, restarting or missing needs no special case.
// Both libraries load at run time without cgo; DCGM's through its host engine on 127.0.0.1:5555. DCGM numbers GPUs
// its own way, so they are matched to NVML's by UUID.
//
//	metrics     Key and DCGM field ID. NVML reads the first eleven.
//	gpu         handle is NVML's.
//	nvmlMemory, nvmlField, nvmlProcess  Mirror nvml.h.
//	reading     One value read, and whether it was.
//	dcgmValue, dcgmEntity  Mirror dcgmFieldValue_v2 and dcgmGroupEntityPair_t.
//	dcgm        The bound DCGM functions.
//	bind, cstr, scalar
//	loadDCGM    Loads libdcgm once.
//	openNVML    Test seam. The GPUs, a metric reader by UUID and a per-process memory reader.
//	openDCGM    Test seam. A reader that answers nil unless every job GPU is found and watched; once it fails it
//	            disconnects and answers nil.
//	openGPUs    A reader answering the source and GPU entries, from NVML whenever DCGM answers nothing, which then
//	            reopens DCGM for the next sample; and a per-process memory reader. Without GPUs they answer nothing.
package usage

import (
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/purego"
)

var metrics = []struct {
	key  string
	dcgm uint16
}{
	{"utilPct", 203}, {"memUtilPct", 204}, {"memUsedMiB", 252}, {"memTotalMiB", 250}, {"powerW", 155},
	{"tempC", 150}, {"memTempC", 140}, {"smClockMHz", 100}, {"memClockMHz", 101}, {"throttleReasons", 112},
	{"eccErrors", 311}, {"energyMJ", 156}, {"xidLast", 230}, {"smActive", 1002}, {"smOccupancy", 1003},
	{"tensorActive", 1004}, {"dramActive", 1005}, {"fp64Active", 1006}, {"fp32Active", 1007}, {"fp16Active", 1008},
	{"pcieTxBytesPerSec", 1009}, {"pcieRxBytesPerSec", 1010}, {"nvlinkTxBytesPerSec", 1011},
	{"nvlinkRxBytesPerSec", 1012},
}

type gpu struct {
	index      int
	uuid, name string
	handle     uintptr
}

type nvmlMemory struct {
	version     uint32
	total, _, _ uint64
	used        uint64
}

type nvmlField struct {
	id, _  uint32
	_, _   int64
	_      uint32
	status uint32
	value  uint64
}

type nvmlProcess struct {
	pid  uint32
	used uint64
	_, _ uint32
}

type reading struct {
	value float64
	ok    bool
}

type dcgmValue struct {
	version     uint32
	_           int32
	entity      uint32
	field, kind uint16
	status      int32
	_           uint32
	_           int64
	value       [4096]byte
}

type dcgmEntity struct {
	group int32
	id    uint32
}

const (
	dcgmFEGPU      = 1
	dcgmFieldUUID  = 54
	dcgmValueSize  = 4128
	dcgmInt64Blank = 0x7ffffffffffffff0
	dcgmFP64Blank  = 140737488355328.0
)

type dcgm struct {
	connect    func(string, *uintptr) int32
	disconnect func(uintptr) int32
	devices    func(uintptr, *[32]uint32, *int32) int32
	latest     func(uintptr, *dcgmEntity, uint32, *uint16, uint32, uint32, *dcgmValue) int32
	group      func(uintptr, int32, string, *uintptr) int32
	add        func(uintptr, uintptr, int32, uint32) int32
	fieldGroup func(uintptr, int32, *uint16, string, *uintptr) int32
	watch      func(uintptr, uintptr, uintptr, int64, float64, int32) int32
	update     func(uintptr, int32) int32
}

func bind(lib string, fns map[string]any) bool {
	h, err := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return false
	}
	for name, fn := range fns {
		sym, err := purego.Dlsym(h, name)
		if err != nil {
			return false
		}
		purego.RegisterFunc(fn, sym)
	}
	return true
}

func cstr(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\x00")
	return s
}

func scalar[T uint32 | uint64](scale float64, call func(*T) int32) reading {
	var v T
	ok := call(&v) == 0
	return reading{float64(v) * scale, ok}
}

var (
	loadDCGM = sync.OnceValue(func() *dcgm {
		var d dcgm
		var initialize func() int32
		if !bind("libdcgm.so.4", map[string]any{
			"dcgmInit":                    &initialize,
			"dcgmConnect":                 &d.connect,
			"dcgmDisconnect":              &d.disconnect,
			"dcgmGetAllSupportedDevices":  &d.devices,
			"dcgmEntitiesGetLatestValues": &d.latest,
			"dcgmGroupCreate":             &d.group,
			"dcgmGroupAddEntity":          &d.add,
			"dcgmFieldGroupCreate":        &d.fieldGroup,
			"dcgmWatchFields":             &d.watch,
			"dcgmUpdateAllFields":         &d.update,
		}) || initialize() != 0 {
			return nil
		}
		return &d
	})
	dcgmOpens atomic.Int64
)

var openNVML = func() ([]gpu, func() map[string]map[string]any, func() map[int]int64) {
	var (
		initialize func() int32
		count      func(*uint32) int32
		handle     func(uint32, *uintptr) int32
		uuid, name func(uintptr, *byte, uint32) int32
		util       func(uintptr, *[2]uint32) int32
		memory     func(uintptr, *nvmlMemory) int32
		power      func(uintptr, *uint32) int32
		temp       func(uintptr, uint32, *uint32) int32
		clock      func(uintptr, uint32, *uint32) int32
		reasons    func(uintptr, *uint64) int32
		ecc        func(uintptr, uint32, uint32, *uint64) int32
		fields     func(uintptr, int32, *nvmlField) int32
		procs      func(uintptr, *uint32, *nvmlProcess) int32
		n          uint32
	)
	if !bind("libnvidia-ml.so.1", map[string]any{
		"nvmlInit_v2":                             &initialize,
		"nvmlDeviceGetCount_v2":                   &count,
		"nvmlDeviceGetHandleByIndex_v2":           &handle,
		"nvmlDeviceGetUUID":                       &uuid,
		"nvmlDeviceGetName":                       &name,
		"nvmlDeviceGetUtilizationRates":           &util,
		"nvmlDeviceGetMemoryInfo_v2":              &memory,
		"nvmlDeviceGetPowerUsage":                 &power,
		"nvmlDeviceGetTemperature":                &temp,
		"nvmlDeviceGetClockInfo":                  &clock,
		"nvmlDeviceGetCurrentClocksEventReasons":  &reasons,
		"nvmlDeviceGetTotalEccErrors":             &ecc,
		"nvmlDeviceGetFieldValues":                &fields,
		"nvmlDeviceGetComputeRunningProcesses_v3": &procs,
	}) || initialize() != 0 || count(&n) != 0 {
		return nil, nil, nil
	}
	gpus := []gpu{}
	for i := range n {
		var h uintptr
		u, m := make([]byte, 96), make([]byte, 96)
		if handle(i, &h) == 0 && uuid(h, &u[0], 96) == 0 && name(h, &m[0], 96) == 0 {
			gpus = append(gpus, gpu{int(i), cstr(u), cstr(m), h})
		}
	}
	read := func(h uintptr) map[string]any {
		var rates [2]uint32
		m, f := nvmlMemory{version: 40 | 2<<24}, nvmlField{id: 82}
		util, mem, field := util(h, &rates) == 0, memory(h, &m) == 0, fields(h, 1, &f) == 0 && f.status == 0
		e := map[string]any{}
		for key, r := range map[string]reading{
			"utilPct":         {float64(rates[0]), util},
			"memUtilPct":      {float64(rates[1]), util},
			"memUsedMiB":      {float64(m.used >> 20), mem},
			"memTotalMiB":     {float64(m.total >> 20), mem},
			"memTempC":        {float64(uint32(f.value)), field},
			"powerW":          scalar(0.001, func(v *uint32) int32 { return power(h, v) }),
			"tempC":           scalar(1, func(v *uint32) int32 { return temp(h, 0, v) }),
			"smClockMHz":      scalar(1, func(v *uint32) int32 { return clock(h, 1, v) }),
			"memClockMHz":     scalar(1, func(v *uint32) int32 { return clock(h, 2, v) }),
			"throttleReasons": scalar(1, func(v *uint64) int32 { return reasons(h, v) }),
			"eccErrors":       scalar(1, func(v *uint64) int32 { return ecc(h, 1, 0, v) }),
		} {
			if r.ok {
				e[key] = r.value
			}
		}
		return e
	}
	return gpus, func() map[string]map[string]any {
			out := map[string]map[string]any{}
			for _, g := range gpus {
				out[g.uuid] = read(g.handle)
			}
			return out
		}, func() map[int]int64 {
			mem, list := map[int]int64{}, make([]nvmlProcess, 1024)
			for _, g := range gpus {
				c := uint32(len(list))
				if procs(g.handle, &c, &list[0]) != 0 {
					return nil
				}
				for _, p := range list[:c] {
					if p.used != ^uint64(0) {
						mem[int(p.pid)] += int64(p.used >> 20)
					}
				}
			}
			return mem
		}
}

var openDCGM = func(gpus []gpu) func() map[string]map[string]any {
	var (
		d        = loadDCGM()
		h, grp   uintptr
		ids      [32]uint32
		n        int32
		entities []dcgmEntity
		fields   []uint16
	)
	none := func() map[string]map[string]any { return nil }
	if d == nil || d.connect("127.0.0.1", &h) != 0 {
		return none
	}
	get := func(entities []dcgmEntity, fields []uint16, flags uint32) []dcgmValue {
		values := make([]dcgmValue, len(entities)*len(fields))
		for i := range values {
			values[i].version = dcgmValueSize | 2<<24
		}
		if d.latest(h, &entities[0], uint32(len(entities)), &fields[0], uint32(len(fields)), flags, &values[0]) != 0 {
			return nil
		}
		return values
	}
	fail := func() func() map[string]map[string]any {
		d.disconnect(h)
		return none
	}
	if d.devices(h, &ids, &n) != 0 || n == 0 {
		return fail()
	}
	for _, id := range ids[:n] {
		entities = append(entities, dcgmEntity{dcgmFEGPU, id})
	}
	byUUID := map[string]uint32{}
	for _, v := range get(entities, []uint16{dcgmFieldUUID}, 1) {
		if v.status == 0 {
			byUUID[cstr(v.value[:])] = v.entity
		}
	}
	name := fmt.Sprintf("linkspan-%d-%d", os.Getpid(), dcgmOpens.Add(1))
	if d.group(h, 1, name, &grp) != 0 {
		return fail()
	}
	entities, uuids := nil, map[uint32]string{}
	for _, g := range gpus {
		id, ok := byUUID[g.uuid]
		if !ok || d.add(h, grp, dcgmFEGPU, id) != 0 {
			return fail()
		}
		entities, uuids[id] = append(entities, dcgmEntity{dcgmFEGPU, id}), g.uuid
	}
	keys := map[uint16]string{}
	for _, m := range metrics {
		fields, keys[m.dcgm] = append(fields, m.dcgm), m.key
	}
	var fg uintptr
	if d.fieldGroup(h, int32(len(fields)), &fields[0], name, &fg) != 0 || d.watch(h, grp, fg, 1_000_000, 0, 2) != 0 {
		return fail()
	}
	d.update(h, 1)
	return func() map[string]map[string]any {
		values := get(entities, fields, 0)
		if values == nil {
			d.disconnect(h)
			return nil
		}
		out := map[string]map[string]any{}
		for _, u := range uuids {
			out[u] = map[string]any{}
		}
		for _, v := range values {
			raw := binary.NativeEndian.Uint64(v.value[:8])
			r := map[uint16]reading{
				'd': {math.Float64frombits(raw), math.Float64frombits(raw) < dcgmFP64Blank},
				'i': {float64(int64(raw)), int64(raw) < dcgmInt64Blank},
			}[v.kind]
			if v.status == 0 && r.ok {
				out[uuids[v.entity]][keys[v.field]] = r.value
			}
		}
		return out
	}
}

func openGPUs() (func() (*string, []map[string]any), func() map[int]int64) {
	gpus, nvml, gpuMem := openNVML()
	if len(gpus) == 0 {
		return func() (*string, []map[string]any) { return nil, nil }, func() map[int]int64 { return map[int]int64{} }
	}
	dcgm := openDCGM(gpus)
	entries := func(source string, values map[string]map[string]any) (*string, []map[string]any) {
		out := []map[string]any{}
		for _, g := range gpus {
			if len(values[g.uuid]) > 0 {
				e := map[string]any{"index": g.index, "uuid": g.uuid, "name": g.name}
				maps.Copy(e, values[g.uuid])
				out = append(out, e)
			}
		}
		return &source, out
	}
	return func() (*string, []map[string]any) {
		if values := dcgm(); values != nil {
			return entries("dcgm", values)
		}
		dcgm = openDCGM(gpus)
		return entries("nvml", nvml())
	}, gpuMem
}
