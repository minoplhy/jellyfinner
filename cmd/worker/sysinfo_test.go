package main

import "testing"

func TestParseCPUInfoX86(t *testing.T) {
	text := ""
	// Two cores, two threads each, on one package.
	for i, c := range []string{"0", "1", "0", "1"} {
		text += "processor\t: " + string(rune('0'+i)) + "\nmodel name\t: AMD Ryzen 7 5800X 8-Core Processor\nphysical id\t: 0\ncore id\t\t: " + c + "\n\n"
	}
	model, cores := parseCPUInfo(text)
	if model != "AMD Ryzen 7 5800X 8-Core Processor" || cores != 2 {
		t.Fatalf("got %q, %d cores", model, cores)
	}
}

func TestParseCPUInfoArm(t *testing.T) {
	text := "processor\t: 0\nBogoMIPS\t: 48.00\nCPU part\t: 0xd05\n\nprocessor\t: 1\nBogoMIPS\t: 48.00\n\nprocessor\t: 2\n\n"
	model, cores := parseCPUInfo(text)
	if model != "" || cores != 3 {
		t.Fatalf("got %q, %d cores", model, cores)
	}
}

func TestParseCPUMax(t *testing.T) {
	for in, want := range map[string]float64{
		"max 100000\n":    0,
		"400000 100000\n": 4,
		"150000 100000":   1.5,
		"":                0,
		"junk":            0,
	} {
		if got := parseCPUMax(in); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}
