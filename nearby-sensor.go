package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/brutella/hc"
	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/characteristic"
	"github.com/brutella/hc/service"
	"github.com/go-ble/ble"
	"github.com/go-ble/ble/examples/lib/dev"
	log "github.com/sirupsen/logrus"
)

// 0 no debug, 1 some debug, 2 all of it
const debug = 1

var apple_company_id = []byte{0x4c, 0x00}
var ble_packets_types = map[byte]string{
	0x03: "airprint",
	0x05: "airdrop",
	0x06: "homekit",
	0x07: "airpods",
	0x08: "siri",
	0x09: "airplay",
	0x10: "nearby",
	0x0b: "watch_c",
	0x0c: "handoff",
	0x0d: "wifi_set",
	0x0e: "hotspot",
	0x0f: "wifi_join",
}

var beacon_ch = make(chan Beacon)

// in: ManufacturerData without header. format: some TLVs
// out: struct with all TLVs
func parse_ble_adv(data []byte) map[byte][]byte {
	parsed_data := make(map[byte][]byte)
	max_i := uint(len(data)) - 1
	var i uint = 0
	for {
		// can we still access the header?
		if i+2 >= max_i {
			break
		}
		tag := data[i]
		i += 1
		val_len := uint(data[i])
		i += 1
		if i+val_len >= max_i {
			// we'd run out of bounds, probably this wasn't a TLV
			break
		}
		parsed_data[tag] = data[i : i+val_len]
		i += val_len
		if i >= max_i {
			break
		}
	}
	return parsed_data
}

type Beacon struct {
	time     time.Time
	RSSI     int
	Services []byte
}

func advHandler(a ble.Advertisement) {
	b := Beacon{}
	if debug == 2 {
		fmt.Printf("Manufacturer Data: 0x%s\n", hex.EncodeToString(a.ManufacturerData()))
		fmt.Printf("Services: %v\n", a.Services())
		for i, s := range a.ServiceData() {
			fmt.Printf("Service %v, UUID: 0x%v, Data: 0x%x\n", i, s.UUID, s.Data)
		}
	}

	if len(a.ManufacturerData()) < 2 || !bytes.Equal(a.ManufacturerData()[0:2], apple_company_id) {
		// not an Apple device
		return
	}

	parsed_data := parse_ble_adv(a.ManufacturerData()[2:])
	b.RSSI = a.RSSI()
	for k, _ := range parsed_data {
		b.Services = append(b.Services, k)
	}
	if debug == 2 {
		fmt.Println(b)
	}
	b.time = time.Now()
	beacon_ch <- b
}

func main() {
	hciDev := flag.String("dev", "hci0", "Bluetooth Device")
	pinFlag := flag.String("pin", "32191123", "HomeKit 8-digit PIN for this accessory")
	thresholdStr := flag.String("threshold", "-50", "Filter beacons below this threshold, in dBm")
	timeoutStr := flag.String("timeout", "5", "Switch-off delay, in seconds")
	var threshold int64
	var timeout int64
	flag.Parse()

	// Bluetooth scanning
	d, err := dev.NewDevice(*hciDev)
	if err != nil {
		log.Fatalf("can't open device: %s", err)
	}
	ble.SetDefaultDevice(d)
	go ble.Scan(context.Background(), true, advHandler, nil)

	if _, err := strconv.ParseUint(*pinFlag, 10, 0); err != nil {
		fmt.Println("PIN must be a number")
	}

	if threshold, err = strconv.ParseInt(*thresholdStr, 10, 0); err != nil {
		fmt.Println("Threshold must be a number")
	}

	if timeout, err = strconv.ParseInt(*timeoutStr, 10, 0); err != nil {
		fmt.Println("Timeout must be a number")
	}

	info := accessory.Info{
		Name:         "Nearby Sensor",
		Manufacturer: "Nearby Sensor",
	}

	acc := accessory.New(info, accessory.TypeSensor)
	service := service.NewContactSensor()
	acc.AddService(service.Service)
	// start HomeKit accessory
	t, err := hc.NewIPTransport(hc.Config{Pin: *pinFlag}, acc)
	fmt.Println("PIN: ", *pinFlag)

	// for "Window Sensors", contact detected == window closed, no contact == window open
	const beaconFound = characteristic.ContactSensorStateContactNotDetected
	const noBeaconFound = characteristic.ContactSensorStateContactDetected
	service.ContactSensorState.SetValue(noBeaconFound)
	timeout_d := time.Duration(timeout) * time.Second

	go func(timeout time.Duration, threshold int64, debug int) {
		// we need to avoid state changes and have some inertia / filter

		for {
			select {
			case b := <-beacon_ch:
				// beacon received
				if debug == 2 {
					fmt.Println(b)
				}
				if b.RSSI < int(threshold) {
					if debug == 2 {
						fmt.Printf("Ignoring beacon: RSSI %v below threshold %v", b.RSSI, threshold)
					}
					continue
				}
				// only consider beacons from the last second or so
				if b.time.Before(time.Now().Add(time.Duration(-2) * time.Second)) {
					fmt.Println("Skipping old beacon")
					continue
				}
				// beacon found
				fmt.Println(b)
				if service.ContactSensorState.GetValue() == noBeaconFound {
					if debug == 1 || debug == 2 {
						fmt.Println("Beacon found, switching sensor to 'BeaconFound'")
					}
					service.ContactSensorState.SetValue(beaconFound)
				} else {
					if debug == 1 || debug == 2 {
						fmt.Println("Beacon found but sensor already in state 'BeaconFound'. Delaying switch to 'NoBeaconFound'")
					}
				}
				time.Sleep(timeout)
			default:
				// only occurs if sleep is done and no beacon was received
				if service.ContactSensorState.GetValue() != noBeaconFound {
					if debug == 1 || debug == 2 {
						fmt.Println("No Beacon found, switching sensor to 'noBeaconFound'")
					}
					service.ContactSensorState.SetValue(noBeaconFound)
				}
			}
		}
	}(timeout_d, threshold, debug)

	if err != nil {
		log.Fatal(err)
	}

	hc.OnTermination(func() {
		<-t.Stop()
	})

	t.Start()
}
