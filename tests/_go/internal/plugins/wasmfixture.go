package plugins

// This file builds a tiny valid core-WASM module by hand so the runtime tests
// need no external WASM toolchain. The guest imports mv_write_output and
// exports the three lifecycle functions plus its memory.

var fixtureResponse = []byte(`{"metrics":[{"name":"fixture.metric","value":1}]}`)

func fixtureModule() []byte {
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	writeFunc := append([]byte{0x60}, vec([][]byte{{0x7f}, {0x7f}})...)
	writeFunc = append(writeFunc, vec([][]byte{{0x7f}})...)
	lifecycleFunc := append([]byte{0x60}, vec(nil)...)
	lifecycleFunc = append(lifecycleFunc, vec([][]byte{{0x7f}})...)
	module = append(module, section(1, vec([][]byte{writeFunc, lifecycleFunc}))...)

	imp := append(nameBytes("env"), nameBytes("mv_write_output")...)
	imp = append(imp, 0x00, 0x00)
	module = append(module, section(2, vec([][]byte{imp}))...)

	module = append(module, section(3, vec([][]byte{{0x01}, {0x01}, {0x01}}))...)
	module = append(module, section(5, vec([][]byte{{0x00, 0x01}}))...)

	exports := [][]byte{
		exportBytes("mv_init", 0, 1),
		exportBytes("mv_handle", 0, 2),
		exportBytes("mv_shutdown", 0, 3),
		exportBytes("memory", 2, 0),
	}
	module = append(module, section(7, vec(exports))...)

	initBody := []byte{0x00, 0x41, 0x00, 0x0b}
	handleBody := []byte{0x00, 0x41, 0x00, 0x41, byte(len(fixtureResponse)), 0x10, 0x00, 0x1a, 0x41, 0x00, 0x0b}
	bodies := [][]byte{
		append(uleb(uint64(len(initBody))), initBody...),
		append(uleb(uint64(len(handleBody))), handleBody...),
		append(uleb(uint64(len(initBody))), initBody...),
	}
	module = append(module, section(10, vec(bodies))...)

	data := []byte{0x00, 0x41, 0x00, 0x0b}
	data = append(data, uleb(uint64(len(fixtureResponse)))...)
	data = append(data, fixtureResponse...)
	module = append(module, section(11, vec([][]byte{data}))...)
	return module
}

func exportBytes(name string, kind byte, index byte) []byte {
	out := nameBytes(name)
	out = append(out, kind, index)
	return out
}

func nameBytes(s string) []byte {
	return append(uleb(uint64(len(s))), []byte(s)...)
}

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint64(len(payload)))...)
	return append(out, payload...)
}

func vec(items [][]byte) []byte {
	out := uleb(uint64(len(items)))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func uleb(n uint64) []byte {
	var out []byte
	for {
		part := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			part |= 0x80
		}
		out = append(out, part)
		if n == 0 {
			return out
		}
	}
}
