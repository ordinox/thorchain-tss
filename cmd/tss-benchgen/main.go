package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/ordinox/thorchain-tss-lib/common"
	"github.com/ordinox/thorchain-tss-lib/ecdsa/keygen"
	"github.com/ordinox/thorchain-tss-lib/test"
	"github.com/ordinox/thorchain-tss-lib/tss"
	"github.com/ipfs/go-log"
	"github.com/pkg/errors"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const libLogLevel = "error"

var (
	expectedIncomingMsgs,
	receivedIncomingMsgs,
	nMinus1 float64
	preParamTestData keygen.LocalPreParams
)

func init() {
	preDataJSON, _ := hex.DecodeString(preParamDataHex)
	if err := json.Unmarshal(preDataJSON, &preParamTestData); err != nil {
		panic(err)
	}
}

func usage() {
	if _, err := fmt.Fprintf(os.Stderr, "usage: tss-benchgen [-flag=value, ...] datadir\n"); err != nil {
		panic(err)
	}
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	prt := message.NewPrinter(language.English)
	var (
		quorum  = flag.Int("q", 2, "the signing quorum (t+1)")
		parties = flag.Int("n", 20, "the number of party shares to generate (n)")
		procs   = flag.Int("procs", runtime.NumCPU(), "the number of max go procs (threads) to use")
	)
	flag.Usage = usage
	if flag.Parse(); !flag.Parsed() {
		usage()
		os.Exit(1)
	}
	if *parties <= 0 || *quorum <= 1 || *parties < *quorum {
		fmt.Println("Error: n must be greater than 0, q must be greater than 1, q cannot be less than n.")
		os.Exit(1)
	}
	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}
	dir := flag.Args()[0]
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		fmt.Printf("Error: `%s` already exists, delete it first and this tool will create it.\n", dir)
		os.Exit(1)
	}
	if err := os.Mkdir(dir, os.ModePerm); err != nil {
		panic(err)
	}

	fmt.Println("ECDSA/GG20 Benchmark Tool - KeyGen")
	fmt.Println("----------------------------------")
	fmt.Printf("Max go procs (threads): %d\n", *procs)
	fmt.Printf("Generating %d shares, quorum=%d...\n", *parties, *quorum)
	fmt.Println("No network latency.")
	fmt.Println("----------------------------------")

	runtime.GOMAXPROCS(*procs)
	start := time.Now()
	runKeyGen(dir, (*quorum)-1, *parties)
	elapsed := time.Since(start)

	fmt.Printf("Done. %d shares written to `%s`.\n", *parties, dir)
	_, _ = prt.Printf("Finished in %d ms.\n", elapsed.Milliseconds())
	os.Exit(0)
}

func setUp(level string) {
	if err := log.SetLogLevel("tss-lib", level); err != nil {
		panic(err)
	}
}

func setUpProgress(n int) {
	nMinus1 = float64(n) - 1
	expectedIncomingMsgs = (3 * nMinus1) + nMinus1
	receivedIncomingMsgs = -1
}

func incrementAndDisplayProgress() {
	var progress float64
	receivedIncomingMsgs++
	if receivedIncomingMsgs > 0 {
		progress = math.Min(1, receivedIncomingMsgs/expectedIncomingMsgs)
	} else {
		progress = 0
	}
	fmt.Printf("\rProgress: %d%%... ", int(progress*100))
}

func runKeyGen(dir string, t, n int) {
	setUp(libLogLevel)
	setUpProgress(n)

	fmt.Printf("Starting... ")

	pIDs := tss.GenerateTestPartyIDs(n)

	p2pCtx := tss.NewPeerContext(pIDs)
	parties := make([]*keygen.LocalParty, 0, len(pIDs))

	errCh := make(chan *tss.Error, len(pIDs))
	outCh := make(chan tss.Message, len(pIDs))
	endCh := make(chan keygen.LocalPartySaveData, len(pIDs))

	updater := test.SharedPartyUpdater

	// init the parties
	for i := 0; i < len(pIDs); i++ {
		params := tss.NewParameters(p2pCtx, pIDs[i], len(pIDs), t)
		params.UNSAFE_setKGIgnoreH1H2Dupes(true)
		P := keygen.NewLocalParty(params, outCh, endCh, preParamTestData).(*keygen.LocalParty)
		parties = append(parties, P)
		go func(P *keygen.LocalParty) {
			if err := P.Start(); err != nil {
				errCh <- err
			}
		}(P)
	}

	// PHASE: keygen
	var ended int32
outer:
	for {
		select {
		case err := <-errCh:
			common.Logger.Errorf("Error: %s", err)
			panic(err)

		case msg := <-outCh:
			dest := msg.GetTo()
			if dest == nil { // broadcast!
				for _, P := range parties {
					if P.PartyID().Index == msg.GetFrom().Index {
						continue
					}
					go updater(P, msg, errCh)
				}
			} else { // point-to-point!
				if dest[0].Index == msg.GetFrom().Index {
					panic(fmt.Errorf("party %d tried to send a message to itself (%d)", dest[0].Index, msg.GetFrom().Index))
				}
				go updater(parties[dest[0].Index], msg, errCh)
			}
			incrementAndDisplayProgress()

		case save := <-endCh:
			// SAVE a test fixture file for this P (if it doesn't already exist)
			// .. here comes a workaround to recover this party's index (it was removed from save data)
			index, err := save.OriginalIndex()
			if err != nil {
				panic(err)
			}
			tryWriteKeyGenDataFile(dir, index, save)

			atomic.AddInt32(&ended, 1)
			if atomic.LoadInt32(&ended) == int32(len(pIDs)) {
				// build ecdsa key pair
				pkX, pkY := save.ECDSAPub.X(), save.ECDSAPub.Y()
				pk := ecdsa.PublicKey{
					Curve: tss.EC(),
					X:     pkX,
					Y:     pkY,
				}
				sk := ecdsa.PrivateKey{
					PublicKey: pk,
				}
				// test pub key, should be on curve and match pkX, pkY
				if !sk.IsOnCurve(pkX, pkY) {
					panic("public key must be on curve, but it was not")
				}
				break outer
			}
		}
	}
}

func tryWriteKeyGenDataFile(dir string, index int, data keygen.LocalPartySaveData) {
	fixtureFileName := makeKeyGenDataFilePath(dir, index)

	// fixture file does not already exist?
	// if it does, we won't re-create it here
	fi, err := os.Stat(fixtureFileName)
	if !(err == nil && fi != nil && !fi.IsDir()) {
		fd, err := os.OpenFile(fixtureFileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			panic(errors.Wrapf(err, "unable to open fixture file %s for writing", fixtureFileName))
		}
		bz, err := json.Marshal(&data)
		if err != nil {
			panic(errors.Wrapf(err, "unable to marshal save data for fixture file %s", fixtureFileName))
		}
		_, err = fd.Write(bz)
		if err != nil {
			panic(errors.Wrapf(err, "unable to write to fixture file %s", fixtureFileName))
		}
		common.Logger.Debugf("Saved a test fixture file for party %d: %s\n", index, fixtureFileName)
	} else {
		fmt.Printf("\nFixture file already exists for party %d; not re-creating: %s\n", index, fixtureFileName)
	}
	//
}

func makeKeyGenDataFilePath(dir string, partyIndex int) string {
	return fmt.Sprintf("%s/keygen_data_%d.json", dir, partyIndex)
}

// ----- //

const (
	// taken from line 1 of `test_data/preParam_test.data` in this repo
	preParamDataHex = "7b225061696c6c696572534b223a7b224e223a32343532323738383739373632383839303737343836303337323738323231363838373837303933323538353339353732353131303336363932313135303332343336383134333636343233333935383336343831343137323034303230303836303933303332383334303536303635313639333439303739313932343837393732313833323431393931393431313330303230393231393331383835333539343536343738323834323634383236313332343934383636313737303137353832373337373931383439393635343839323035373238343232373335373931363134353830343236353139353733333334313535303739393632333432383737303536343630333534393837303336393032303634373835373034393434313035323538373734313633353032373232383332303238313739383833373133313638373531313731363935363336353239353230373239343230333233393137353930303537363532373430303731393139393335303536333533313735343232393036313433303334393533353136343534313331393338303335333037323434323534333135363937363236353133323739303135373338303939303833323934303734393733373934393138303536313837333634353632303537353635393739353130303830363432393538373533313731323734333531373232373737393330363933353536393637323132393834373334323737373836343731383239383539333835383639303032303739393434343734343031353839373830323131323431303430393730343935393438343531333333373132333038343137363039353939332c224c616d6264614e223a31323236313339343339383831343434353338373433303138363339313130383434333933353436363239323639373836323535353138333436303537353136323138343037313833323131363937393138323430373038363032303130303433303436353136343137303238303332353834363734353339353936323433393836303931363230393935393730353635303130343630393635393432363739373238323339313432313332343133303636323437343333303838353038373931333638383935393234393832373434363032383634323131333637383935383037323930323133323539373836363637303737353339393831313731343338353238323330313737343933353138343531303332333932383532343732303532363239333837303831373531333631343135393938333333333231333432333931303636323636303535313630383935373438323830343134373935363938303736393933383936303136303532323131333134303938313434303834373430343530373834333936333332363532363138383134373132323339313738323033303031353334333531373339393735313934393933323031353831333937373332313633383538363432323038353733303339323239363435323233313133323638333036393539353430333435353237323233323133303538393732333732343734323330363137343936353735393333313034383931353637313433373338363938343031323834333732343937393637303138313132303639353332303832373231393433373635373636393136333834313530343032373233363737373231333837342c225068694e223a32343532323738383739373632383839303737343836303337323738323231363838373837303933323538353339353732353131303336363932313135303332343336383134333636343233333935383336343831343137323034303230303836303933303332383334303536303635313639333439303739313932343837393732313833323431393931393431313330303230393231393331383835333539343536343738323834323634383236313332343934383636313737303137353832373337373931383439393635343839323035373238343232373335373931363134353830343236353139353733333334313535303739393632333432383737303536343630333534393837303336393032303634373835373034393434313035323538373734313633353032373232383331393936363636363432363834373832313332353332313130333231373931343936353630383239353931333936313533393837373932303332313034343232363238313936323838313639343830393031353638373932363635333035323337363239343234343738333536343036303033303638373033343739393530333839393836343033313632373935343634333237373137323834343137313436303738343539323930343436323236353336363133393139303830363931303534343436343236313137393434373434393438343631323334393933313531383636323039373833313334323837343737333936383032353638373434393935393334303336323234313339303634313635343433383837353331353333383332373638333030383035343437333535343432373734387d2c224e54696c646569223a32303334323336343032373333323235393235333237333330333138363433363634383532373832373339383132373939373135353036343731363631373839373035383335353939343331393038353438363832383539383737313632323633303830323535323839363738333632343835303738303337383738323037353939383335313237303336323330313739333037313733343937363332313735353135383731363534333638393330353237353038363638363536353736373032353431373730343735313533343330303138313334383235333336353434373430373639323035353134313139393530363330363639383538353737393136303638353036353932343639383338323136353536363932303936313636383234383834383438383432333738373439393034383330393032383031303333323939343831323531363033383332393339353133313037303936383232343330373339373735393235393934323430313738353335333531393638343836353234363437333034393438333234373732373036393039373836333337393536373137303138313131333730363839323733373933363335333631373039323436303330313435383639333735303839343133383135343436333336343038363537363933363833333939373535373832323031373838393238323739393535373532383033363634313233353337313637393635323834333233393133343238303332393634323134363036303633393537373737313535323530303830313633333633383233363039383733343535353839343038343133323231313731373939363835363533372c22483169223a3237323134393238353633313739353634313036343531333039363134343038383830303539333230363532373133363138393735323134313734323836343538343437373432333136373739373336303737313634373031373639363836373136333032353136393338353633353339313739353938393134353139323336393431353238353830373039323330393031313734313636383932303732343835353932343434323134303936343838383932323337393236383633333839323439383439323836353936363730333336393134353537303436303538333238383137323432333630313639363935373134353934353330343030383137303639333632353932373131323230373537343934303034353738373731333439353437363837363934313839383936333537313336393335383439343332313839343135313037323330323230303433383239363438373136373836343937363032393633363631323234323939373639313631323730333439363739333336313134383833343433353936373538393532323536383030323432323037393738333339333234363833353633323330303938383032323834383635313037313434383336343237373634363731333237373034313730313236303535363331363635373334323638343730303233353339353539333630333535353930343537343336383036333539303731383534333539393732383338323431363531353231383538373636303338383739383938313237323234363437353533343735393839323036343536323133343636353433303630333430313837343339393832353030373732322c22483269223a383433393233363237383030363638373738303637333832323833353535363636363733353935333934313531383733393936363532363239363735393535333538303730313335343734393736353031373934373933383834303633383735353833373537333031383633323934343237343236383038313137383837303030303730393931353731353839313733353330373933373339343030383133373830303035313339303339303632323131323638393035323830343334323237323930383634333939303036323539303831393636373939343731313234393535303433303134383333333037323235353639313333313237303935393533363131333338303531313637393438313730363738373139353034363437393333353134333230323737333434383539393037383530323933363030373832343736393535363837323730323639353936363136313539383039393132313332363237363330313736343431363439343230343832383935373439313136393037363536343037313338393231333638383834393632393732343630363135333633333933383933393938333636323938333438363332383730353137313537353732373534303130363439333134353336363639353035373736343530303031303639393634323536363435323338353035303437393234303238303936393938323933313034383439333331353932323937323734373531343231363833303238393530323339333531323436343336393535393239333231383936373131313337383338303936303930303038303136303738343038383732333037313637343733393334312c22416c706861223a313732373737373239363533393831373034393831373934383038373334373236313830383632353037393132353532363936393038323438313436353834383332353536373038393837363435383438323430383334343535313634303131373331323937393133343636373531363735383231383038373537363139313331363638383539333736323035313738363732313237343434383837343832393739333935383932363736313033383836373135373334353735393337303837363835323334313935363134323433383036313333363432383635313334313934393133323734383730313830343532333338383134353134383334333537343632333036323334303938383034333037363635383432393336393037313234373137343939363233323035373830363338333339343334353935313930303539393431363530323736363830353831323638343538363133353132363732363438353338363033323839333434383631313737373333343733343830313635393631323438353439313930383636313037313837333036343839303338313531393037333637333033373836393334333338363538353431303733353731303337333434333739373338313633333834343834363233393430393230333533393535343839343732393036383032373735363135303933343735383436333433313636383637363535373032353731353131333330333730353935333135353936313735303335313338323835393236363534383538313038373333353734303735383937303839373335343432313535303739313632393831303430393136343034313335342c2242657461223a313035303835313535333933353538303834343731313530383930303234333336363036323638363536393639343733393937373836383333333332343430313132383938383030353234343031363431343836393432303734363930343235383432373939383639313132393035373138353038353438373634353730373736323935363431353636363039383130343334373033313231363034383930393139383630333630343836313130343138393832383139363734323631333330353331313537363134313238323233303837333138303831303332373538353839303836373833313933353738383431343333363738393830343139333236393439313833313633333939313234343531343531333432303434393638353530303236363630363234313738353332303532333938333835353333373133333639303932353136303834383837353535353334333534363135323138323036323533313830323737313238393932363132303837353733303932303131353730393232373631333731333336313034303534383132313736383630343231383938393038393030363030303731363531393731383937323735323332323631393136353334333231383030323832353931333834353434353838363839353534353834333435313039363034383338393337323432333136363530323337393433353638393137323535303131323035343432323239353330373831373235313438323937353133323731323532383830333338343735313036323634373139303535363733363136363935343333333436393533333131333835323938333135313936353836362c2250223a36393233333439373539363433373138343336363130343532363230383535343736373538333135323734393438303433343437313332343035313933383135303039383937393736313837393530353838353331363838333833333931363333383738353831323939353731343235353436373531323936323932343631363632303537353431363538333832363733303732303232373130323039393536303738343931393238373535383339363036303031353734373535303135393234383434303332363032353430313432373632383935313839333732343839323533313236363033333833353631313135393035313930353236333036323634303330383137383137373033393733353939373034323937333230353335393332333634383038363733373936333636313835392c2251223a37333435353634313839383632323938323739353433343636343839363038333931353136373332393034393535343932373734323738373335373436353838323030303733333631383936393536343830303333303936333132393538373837323037353035393938373835343438393035303136323537313138303439333837303331363237313233393836363438303835393633373730313234393239373834313635333733373535303738353938343330343238313332373731393232303132383035323639323433363537313833383831343531313833303137393636363032333831363635303637303933353937353632303437343033353831363730353537373336353630373139303533373838363534363734353139333839393532303138353436333932363133343531317d"
)
