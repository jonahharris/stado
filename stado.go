/*
# ------------------------------------------------------------------------------
#
#  Copyright 2019 Kamil Stawiarski ( kstawiarski@ora-600.pl | http://ora-600.pl )
#		  Radoslaw Kut     ( rkut@ora-600.pl )
#  Database Whisperers sp. z o. o. sp. k.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# ------------------------------------------------------------------------------
*/

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
	"bytes"
	"strconv"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/ora600pl/stado/sqlid"
	"github.com/wcharczuk/go-chart"
	"github.com/wcharczuk/go-chart/drawing"
)

func StdDev(x []float64) float64 {
	var sum, mean, sd float64
	for _, elem := range x {
		sum += elem
	}
	mean = sum / float64(len(x))
	for _, elem := range x {
		sd += math.Pow(elem-mean, 2)
	}

	sd = math.Sqrt(sd / float64(len(x)))
	return sd
}

type SQLtcp struct {
	SQL_id       string
	SQL          string
	Conversation string
	Payload      []byte
	Seq          uint32
	Ack          uint32
	Timestamp    time.Time
	IsReused     uint
	RTT	     int64
}

type SQLtcpSort []SQLtcp

func (a SQLtcpSort) Len() int           { return len(a) }
func (a SQLtcpSort) Less(i, j int) bool { return a[j].Seq == a[i].Ack }
func (a SQLtcpSort) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

var Conversations map[string][]SQLtcp

type SQLstats struct {
	SQLtxt         string
	Elapsed_ms_all []float64 //Elapsed time from net perspective (RTT) Net Time 
	Elapsed_ms_sum float64 //All elapsed times from net perspective per packet (RTT) Net Time
	Executions     uint
	Packets        uint
	Sessions       map[string]uint //key: ConversationId
	ReusedCursors  uint
	Elapsed_ms_app float64 //Wallclock from the whole app perspective
	Ela_ms_app_all []float64 //Elapsed time from app perspective
}

func (s *SQLstats) Fill(sqlTxt string, sqlDuration int64, session string, packet_cnt uint, reusedCursors uint, sqlApp int64) {
	s.SQLtxt = sqlTxt
	s.Elapsed_ms_all = append(s.Elapsed_ms_all, float64(sqlDuration)/1000000)
	s.Elapsed_ms_sum += float64(sqlDuration) / 1000000
	s.Executions += 1
	s.Packets += packet_cnt
	s.Sessions[session] = 1
	s.ReusedCursors += reusedCursors
	s.Elapsed_ms_app += float64(sqlApp) / 1000000
	s.Ela_ms_app_all = append(s.Ela_ms_app_all, float64(sqlApp)/1000000)
}

var SQLIdStats map[string]*SQLstats

func banner() {
	fmt.Println("STADO (SQL Tracedump Analyzer Doing Oracle) by Radoslaw Kut and Kamil Stawiarski")
	fmt.Println("Pcap file analyzer for finding TOP SQLs from an APP perspective")
}

func main() {
	pcapFile := flag.String("f", "", "path to PCAP file for analyzing")
	dbIP := flag.String("i", "", "IP address of database server")
	dbPort := flag.String("p", "", "Listener port for database server")
	debug := flag.Int("d", 0, "Debug flag")
	chartsDir := flag.String("C", "", "<dir> directory path to write SQL Charts i.e. -C DevApp")

	flag.Parse()

	if *pcapFile == "" || *dbIP == "" || *dbPort == "" {
		banner()
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *debug == 0 {
		log.SetOutput(ioutil.Discard)
	}

	if *chartsDir == "" {
		*chartsDir = "./SQLCharts"
		if _, err := os.Stat(*chartsDir); os.IsNotExist(err) {
			err = os.Mkdir(*chartsDir, 0755)
			if err != nil {
				fmt.Println(err)
				os.Exit(2)
			}
			fmt.Println("All SQL Charts will be saved into " + *chartsDir + " dierectory\n")
		}
	} else if _, err := os.Stat(*chartsDir); os.IsNotExist(err) {
		err = os.Mkdir(*chartsDir, 0755)
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
	}

	dbIPs := strings.Split(*dbIP, "or")
	log.Println("dB IPs for check: ", dbIPs)

	Conversations = make(map[string][]SQLtcp)
	SQLIdStats = make(map[string]*SQLstats)

	SQLslot := make(map[string]string)
	//reqTimestamp := make(map[string] time.Time)
	//resTimestamp := make(map[string] time.Time)
	ipTnsBytes := make(map[string] uint64)

	handle, err := pcap.OpenOffline(*pcapFile)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Opened pcap file")
	defer handle.Close()

	filter := "host " + *dbIP + " and port " + *dbPort
	err = handle.SetBPFFilter(filter)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Created BPF Filter", filter)

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	rSQL := regexp.MustCompile("(?i)SELECT|update|insert|with|delete|commit|alter")
	log.Println("Created regular expression for SQLs")

	var appPort, appIp, sqlTxt, found_dbIp, found_dbPort string

	littleEndianFlag := byte(254)
	bigEndianFlag := byte(0)
	oneByteSizeFlag := byte(1)
	uncertainSqlSize := 65279 // 0xFEFF at the beginning of SQL
	usedCursorFlag := []byte{29, 6} //Packet length 29 and type DATA (0x06)
	usedCursorFlagAfterError := []byte{48, 6} //Packet length 48 and type DATA (0x06)
	endOfDataFlag := []byte{123, 5} //Flag in ResonseData 0x7b05 before ORA-01403 at the end of fetch
	retOpiParam := byte(8) //TNS Header at @10
	retStatus := byte(4) //TNS Header at @10
	tnsPacketData := byte(6) //TNS Header at@4

	sqlTxtFlow := make(map[string] string)

	var tBegin, tEnd time.Time
	reusedCursor := uint(0)

	for packet := range packetSource.Packets() {
		log.Println("Started packets loop")
		if app := packet.ApplicationLayer(); app != nil {
			tcpLayer := packet.Layer(layers.LayerTypeTCP)
			ipv4Layer := packet.Layer(layers.LayerTypeIPv4)
			log.Println("Created tcp and ipv4 layers from packet")
			tcp := tcpLayer.(*layers.TCP)
			ipv4 := ipv4Layer.(*layers.IPv4)
			sqlTxt = "_"
			//log.Println(packet)
			log.Println("Created tcp and ipv4 fields based on layers")
			foundValidPacket := true //flag to filter out packets for testing purposes
			responsePacket := false
			for _, checkIP := range dbIPs {
				log.Println("Checking if " + ipv4.SrcIP.String() +
						" or " + ipv4.DstIP.String() + " contains " + string(checkIP))

				if strings.Contains(ipv4.SrcIP.String(), strings.TrimSpace(checkIP)) {
					log.Println("Database ip: " + string(checkIP) + " found in source")
					appPort = tcp.DstPort.String()
					appIp = ipv4.DstIP.String()
					found_dbIp = ipv4.SrcIP.String()
					found_dbPort = tcp.SrcPort.String()
				} else if strings.Contains(ipv4.DstIP.String(), strings.TrimSpace(checkIP)) {
					log.Println("Database ip: " + string(checkIP) + " found in destination")
					appPort = tcp.SrcPort.String()
					appIp = ipv4.SrcIP.String()
					found_dbIp = ipv4.DstIP.String()
					found_dbPort = tcp.DstPort.String()
				}

			}
			log.Println("Defined app and db ports")
			conversationId := found_dbIp + ":" + found_dbPort + "<->" + appIp + ":" + appPort
			log.Println("Created conversation id", conversationId, tcp.Seq, tcp.Ack)

			ipTnsBytes[found_dbIp] += uint64(len(app.Payload()))
			log.Println("TNS bytes sent over IP address: ", ipTnsBytes)

			if strings.Contains(tcp.DstPort.String(), *dbPort) {
				//reqTimestamp[conversationId] = packet.Metadata().Timestamp
				if mi := rSQL.FindStringIndex(string(app.Payload())); mi != nil &&
					!strings.Contains(string(app.Payload()), "DESCRIPTION") {

					sqlLen := 0
					endianFlag := app.Payload()[mi[0]-5 : mi[0]-4]
					log.Println("Endian flag is: ", endianFlag)
					sqlLenB := app.Payload()[mi[0]-4 : mi[0]]
					log.Println("SQL len is: ", sqlLenB)
					log.Println(packet)

					if endianFlag[0] == littleEndianFlag {
						sqlLen = int(binary.LittleEndian.Uint32(sqlLenB))
					} else if endianFlag[0] == bigEndianFlag {
						sqlLen = int(binary.BigEndian.Uint32(sqlLenB))
					} else if endianFlag[0] == oneByteSizeFlag {
						sqlLen = int(sqlLenB[3])
					}

					if sqlLen == uncertainSqlSize || sqlLen >= len(app.Payload()[mi[0]-4:]) {
						log.Println("Can't determine sqlLen size")
						sqlBufStart := app.Payload()[mi[0]:]
						sqlTxtEnd := len(sqlBufStart)-1
						for i, v := range(sqlBufStart) {
							if int(v) == 0 {
								sqlTxtEnd = i
								break
							}
						}
						sqlTxt = string(sqlBufStart[0:sqlTxtEnd])
					} else {
						sqlTxt = string(app.Payload()[mi[0] : mi[0]+sqlLen])
					}
					sqlTxtFlow[conversationId] = sqlTxt

					log.Println("SQLFlow for conversation ",
						conversationId, sqlTxtFlow[conversationId], sqlid.Get(sqlTxt))

					log.Println("Found SQL Text based on regular expression")
					foundValidPacket = true

				} else if len(app.Payload()) > 13 && (bytes.Equal(app.Payload()[3:5], usedCursorFlag) ||
					  bytes.Equal(app.Payload()[3:5], usedCursorFlagAfterError)) {

					log.Printf("Used: % 02x => %s, %d\n", app.Payload()[3:5], appPort, tcp.Seq)

					cursorSlot := strconv.Itoa(int(app.Payload()[13]))
					sqlTxt = SQLslot[conversationId + "_" + cursorSlot]

					log.Println("Called SQL text from reused cursor: ",
							sqlTxt, appPort, tcp.Seq, tcp.Ack, conversationId + "_" + cursorSlot)

					reusedCursor = 1
					foundValidPacket = true
				}
			} else {
				//resTimestamp[conversationId] = packet.Metadata().Timestamp
				responsePacket = true
				if strings.Contains(string(app.Payload()), "ORA-01403") {

					sqlTxt = "SQL_END"
					endOfDataI := bytes.Index(app.Payload(), endOfDataFlag)
					log.Println("End Of Data Byte is: ", endOfDataI)
					cursorSlot := strconv.Itoa(int(app.Payload()[endOfDataI+6]))
					log.Println("Cursor Slot is: ", cursorSlot)

					/*if _, present := SQLslot[conversationId + "_" + cursorSlot]; !present {
                                                        SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
                                                        sqlTxtFlow[conversationId] = "_"
                                        }*/
					SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
					foundValidPacket = true

				} else if len(app.Payload()) > 20 &&
					  !strings.Contains(string(app.Payload()), "AUTH") &&
					  app.Payload()[4] == tnsPacketData {

					if app.Payload()[10] == retOpiParam {
						cursorSlot := strconv.Itoa(int(app.Payload()[21]))
						log.Println("Cursor Slot in RetOpiParam is: ", cursorSlot, appPort, tcp.Seq)

						/*if _, present := SQLslot[conversationId + "_" + cursorSlot]; !present {
							SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
							log.Println("Set slot ", conversationId + "_" + cursorSlot, " to: " ,
									sqlTxtFlow[conversationId])

							sqlTxtFlow[conversationId] = "_"
						}*/
						SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
						foundValidPacket = true

					} else if app.Payload()[10] == retStatus {

						cursorSlot := strconv.Itoa(int(app.Payload()[28]))
                                                log.Println("Cursor Slot in RetStatus is: ", cursorSlot, appPort, tcp.Seq)

						/*if _, present := SQLslot[conversationId + "_" + cursorSlot]; !present {
                                                        SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
							log.Println("Set slot ", conversationId + "_" + cursorSlot, " to: " ,
									sqlTxtFlow[conversationId])

                                                        sqlTxtFlow[conversationId]= "_"
                                                }*/
						SQLslot[conversationId + "_" + cursorSlot] = sqlTxtFlow[conversationId]
						foundValidPacket = true

					}
				}
			}

			if foundValidPacket {
				if len(sqlTxt) == 0 {
					sqlTxt = "_"
				}
				if tBegin.IsZero() {
					tBegin = packet.Metadata().Timestamp
				}
				tEnd = packet.Metadata().Timestamp

				rtt := int64(0)
				if responsePacket && len(Conversations[conversationId]) >= 1{
					lastIdx := len(Conversations[conversationId]) - 1
					rtt = packet.Metadata().Timestamp.Sub(Conversations[conversationId][lastIdx].Timestamp).Nanoseconds()
				}

				Conversations[conversationId] = append(Conversations[conversationId], SQLtcp{SQL: sqlTxt,
					SQL_id:       sqlid.Get(sqlTxt),
					Conversation: conversationId,
					Payload:      app.Payload(),
					Seq:          tcp.Seq,
					Ack:          tcp.Ack,
					Timestamp:    packet.Metadata().Timestamp,
					IsReused:     reusedCursor,
					RTT:	      rtt,
				})
				log.Println("Added packaet to conversation ID: " +
						conversationId, sqlTxt, sqlid.Get(sqlTxt), len(sqlTxt), reusedCursor, rtt)
				reusedCursor = 0
			}
		}
	}

	for c := range Conversations {
		log.Println(c)
		//sort.Sort(SQLtcpSort(Conversations[c]))
		var tB, tE, tPrev time.Time
		var sqlDuration, packetDuration time.Duration
		sqlTxt := "+"
		sqlId := "+"
		pcktCnt := uint(0)
		RTT := int64(0)
		reusedCursors := uint(0)

		for _, p := range Conversations[c] {
			log.Println(p.SQL_id, p.Seq, p.Ack, p.RTT, RTT, p.Timestamp, string(p.SQL[0]), "...")
			if tPrev.IsZero() {
				tPrev = p.Timestamp
				packetDuration = p.Timestamp.Sub(tPrev)
			} else {
				packetDuration = p.Timestamp.Sub(tPrev)
			}
			pcktCnt += 1
			//RTT += p.RTT
			if p.SQL != "_" && p.SQL != "SQL_END" {
				tB = p.Timestamp
				sqlTxt = p.SQL
				sqlId = p.SQL_id
				reusedCursors += p.IsReused
			} else { //count RTT minus first packet from first response => avoid counting DB Time from first SQL execution
				RTT += p.RTT
			}
			if sqlId != "+" && (p.SQL == "SQL_END" ||
				(len(sqlTxt) > 1 && p.SQL == "_" && sqlTxt[0] != 's' && sqlTxt[0] != 'S')) {

				tE = p.Timestamp
				//sqlDuration = tE.Sub(tB)
				sqlDuration = packetDuration //Valid SQL duration from app perspective (wallclock)
				log.Println("\tsummary: ", sqlDuration.Nanoseconds(), tE.Sub(tB).Nanoseconds(), tB, tE, RTT, sqlId)

				if _, ok := SQLIdStats[sqlId]; !ok {
					SQLIdStats[sqlId] = &SQLstats{SQLtxt: "",
						Elapsed_ms_sum: 0, Executions: 0, Packets: 0,
						Sessions: make(map[string]uint), ReusedCursors: 0,
						Elapsed_ms_app: 0}
				}
				if RTT >= 0 { // Checking if RTT is calculated properly
					SQLIdStats[sqlId].Fill(sqlTxt, RTT, c, pcktCnt, reusedCursors, sqlDuration.Nanoseconds())
				} else {
					log.Println(RTT, sqlTxt, c, sqlId)
				}
				sqlTxt = "+"
				sqlId = "+"
				pcktCnt = 0
				RTT = 0
				tPrev = time.Time{}
				tB = time.Time{}
				tE = time.Time{}
				reusedCursors = 0
			}
		}
	}
	log.Println("Starting to disaplay SQLstats - len: ", len(SQLIdStats))
	fmt.Println("SQL ID\t\tEla App (ms)\tEla Net(ms)\tExec\tEla Stddev App\tEla App/Exec\tEla Stddev Net\tEla Net/Exec\tP\tS\tRC")
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------------------------\n")
	var graphVal []chart.Value
	var sumApp, sumNet float64
	for sqlid := range SQLIdStats {
		fmt.Printf("%s\t%f\t%f\t%d\t%f\t%f\t%f\t%f\t%d\t%d\t%d\n", sqlid,
			SQLIdStats[sqlid].Elapsed_ms_app,
			SQLIdStats[sqlid].Elapsed_ms_sum,
			SQLIdStats[sqlid].Executions,
			StdDev(SQLIdStats[sqlid].Ela_ms_app_all),
			SQLIdStats[sqlid].Elapsed_ms_app/float64(SQLIdStats[sqlid].Executions),
			StdDev(SQLIdStats[sqlid].Elapsed_ms_all),
			SQLIdStats[sqlid].Elapsed_ms_sum/float64(SQLIdStats[sqlid].Executions),
			SQLIdStats[sqlid].Packets,
			len(SQLIdStats[sqlid].Sessions),
			SQLIdStats[sqlid].ReusedCursors)

		sumApp += SQLIdStats[sqlid].Elapsed_ms_app
		sumNet += SQLIdStats[sqlid].Elapsed_ms_sum

		graphVal = append(graphVal, chart.Value{Value:
				SQLIdStats[sqlid].Elapsed_ms_sum /
						float64(SQLIdStats[sqlid].Executions), Label: sqlid})

		var execs []float64
		for exec := 0; exec < int(SQLIdStats[sqlid].Executions); exec++ {
			execs = append(execs, float64(exec))
		}
		SQLgraph := chart.Chart{
			Title: sqlid + " elapsed time per execution (ms)",
			Background: chart.Style{
				Padding: chart.Box{
					Top:    40,
					Bottom: 10,
				},
			},
			Series: []chart.Series{
				chart.ContinuousSeries{
					Style: chart.Style{
						StrokeColor: drawing.ColorRed,               // will supercede defaults
						FillColor:   drawing.ColorRed.WithAlpha(64), // will supercede defaults
					},
					XValues: execs,
					YValues: SQLIdStats[sqlid].Elapsed_ms_all,
				},
			},
		}

		f, err := os.Create(*chartsDir + "/" + sqlid + ".png")
		if err != nil {
			log.Println(err)
		}
		SQLgraph.Render(chart.PNG, f)
		f.Close()
	}

	fmt.Println("\nSum App Time(s):", sumApp/1000)
	fmt.Println("Sum Net Time(s):", sumNet/1000, "\n")

	for ip := range(ipTnsBytes) {
		fmt.Println(ip, ipTnsBytes[ip]/1024, "kb")
	}

	fmt.Println("\n\n\tTime frame: ", tBegin, " <=> ", tEnd)
	fmt.Println("\tTime frame duration (s): ", tEnd.Sub(tBegin).Seconds(), "\n")

	graph := chart.BarChart{
		Title: "SQLid Elapsed Time Summary (ms)",
		Background: chart.Style{
			Padding: chart.Box{
				Top:    100,
				Bottom: 70,
			},
		},
		Height:   1024,
		Width:    2000,
		BarWidth: 7,
		XAxis:    chart.Style{TextRotationDegrees: 90.0},
		Bars:     graphVal, //[]chart.Value of Value: Label:
	}

	f, err := os.Create(*chartsDir + "/" + "_sql_ela_exec.png")
	if err != nil {
		log.Println(err)
	}
	graph.Render(chart.PNG, f)
	f.Close()

}
