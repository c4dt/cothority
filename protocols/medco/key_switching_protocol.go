package medco

import (
	"errors"
	"github.com/dedis/cothority/lib/dbg"
	"github.com/dedis/cothority/lib/network"
	"github.com/dedis/cothority/lib/sda"
	. "github.com/dedis/cothority/services/medco/structs"
	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/random"
)

const KEY_SWITCHING_PROTOCOL_NAME = "KeySwitching"

type KeySwitchedCipherMessage struct {
	Data                  []KeyValCV
	NewKey                abstract.Point
	OriginalEphemeralKeys []KeyValSPoint
	Proof                 [][]CompleteProof
}

type KeySwitchedCipherStruct struct {
	*sda.TreeNode
	KeySwitchedCipherMessage
}

func init() {
	network.RegisterMessageType(KeySwitchedCipherMessage{})
	sda.ProtocolRegisterName(KEY_SWITCHING_PROTOCOL_NAME, NewKeySwitchingProtocol)
}

type KeySwitchingProtocol struct {
	*sda.TreeNodeInstance

	// Protocol feedback channel
	FeedbackChannel chan map[TempID]CipherVector

	// Protocol communication channels
	PreviousNodeInPathChannel chan KeySwitchedCipherStruct

	// Protocol state data
	nextNodeInCircuit *sda.TreeNode
	TargetOfSwitch    *map[TempID]CipherVector
	TargetPublicKey   *abstract.Point
	originalEphemKeys map[TempID][]abstract.Point
}

func NewKeySwitchingProtocol(n *sda.TreeNodeInstance) (sda.ProtocolInstance, error) {
	keySwitchingProtocol := &KeySwitchingProtocol{
		TreeNodeInstance: n,
		FeedbackChannel:  make(chan map[TempID]CipherVector),
	}

	if err := keySwitchingProtocol.RegisterChannel(&keySwitchingProtocol.PreviousNodeInPathChannel); err != nil {
		return nil, errors.New("couldn't register data reference channel: " + err.Error())
	}

	var i int
	var node *sda.TreeNode
	var nodeList = n.Tree().List()
	for i, node = range nodeList {
		if n.TreeNode().Equal(node) {
			keySwitchingProtocol.nextNodeInCircuit = nodeList[(i+1)%len(nodeList)]
			break
		}
	}

	return keySwitchingProtocol, nil
}

// Starts the protocol
func (p *KeySwitchingProtocol) Start() error {

	if p.TargetOfSwitch == nil {
		return errors.New("No ciphertext given as key switching target.")
	}

	if p.TargetPublicKey == nil {
		return errors.New("No new public key to be switched on provided.")
	}

	dbg.Lvl1(p.Entity(), "started a Key Switching Protocol")

	initialMap := make(map[TempID]CipherVector, len(*p.TargetOfSwitch))
	p.originalEphemKeys = make(map[TempID][]abstract.Point, len(*p.TargetOfSwitch))
	for k := range *p.TargetOfSwitch {
		initialCipherVector := *InitCipherVector(p.Suite(), len((*p.TargetOfSwitch)[k]))
		p.originalEphemKeys[k] = make([]abstract.Point, len((*p.TargetOfSwitch)[k]))
		for i, c := range (*p.TargetOfSwitch)[k] {
			initialCipherVector[i].C = c.C
			p.originalEphemKeys[k][i] = c.K
		}
		initialMap[k] = initialCipherVector
	}

	p.sendToNext(&KeySwitchedCipherMessage{
		MapToSliceCV(initialMap),
		*p.TargetPublicKey,
		MapToSliceSPoint(p.originalEphemKeys),
		[][]CompleteProof{}})

	return nil
}

// Dispatch is an infinite loop to handle messages from channels
func (p *KeySwitchingProtocol) Dispatch() error {

	keySwitchingTarget := <-p.PreviousNodeInPathChannel

	origEphemKeys := SliceToMapSPoint(keySwitchingTarget.OriginalEphemeralKeys)

	randomnessContrib := p.Suite().Secret().Pick(random.Stream)
	//keySwitchingTarget.KeySwitchedCipherMessage.Proof = [][]CompleteProof{}
	length := len(keySwitchingTarget.KeySwitchedCipherMessage.Proof)
	newProofs := [][]CompleteProof{}
	dbg.LLvl1("ICI")
	for i, kv := range keySwitchingTarget.Data {
		//dbg.LLvl1(kv.Key)
		if PROOF {
			if length != 0 {
				for u := 0; u < len(kv.Val); u++ {
					if !VectSwitchCheckProof(keySwitchingTarget.KeySwitchedCipherMessage.Proof[0]) {

						dbg.Errorf("ATTENTION, false proof detected")
					}
					keySwitchingTarget.KeySwitchedCipherMessage.Proof = keySwitchingTarget.KeySwitchedCipherMessage.Proof[1:]
				}
			}
		}

		//cv.SwitchForKey(p.Suite(), p.Private(), origEphemKeys[kv.Key], keySwitchingTarget.NewKey, randomnessContrib)
		keySwitchNewVec := SwitchForKey2(kv.Val, p.Suite(), p.Private(), origEphemKeys[kv.Key], keySwitchingTarget.NewKey, randomnessContrib)

		if PROOF {
			dbg.LLvl1("proofs creation")
			dbg.LLvl1("newProofs ", len(newProofs))
			if len(newProofs) == 0 {
				newProofs = [][]CompleteProof{[]CompleteProof{}}
			} else {
				newProofs = append(newProofs, []CompleteProof{})
			}
			//dbg.LLvl1(i)
			newProofs[i] = VectSwitchKeyProof(p.Suite(), p.Private(), randomnessContrib, origEphemKeys[kv.Key], keySwitchingTarget.NewKey, kv.Val, keySwitchNewVec)
			//dbg.LLvl1(newProofs[i])
		}
		keySwitchingTarget.Data[i].Val = keySwitchNewVec

	}

	keySwitchingTarget.Proof = newProofs
	if p.IsRoot() {
		dbg.Lvl1(p.Entity(), "completed key switching.")
		p.FeedbackChannel <- SliceToMapCV(keySwitchingTarget.Data)
	} else {
		dbg.Lvl1(p.Entity(), "carried on key switching.")
		p.sendToNext(&keySwitchingTarget.KeySwitchedCipherMessage)
	}

	return nil
}

// Sends the message msg to the next node in the circuit based on the next TreeNode in Tree.List() If not visited yet.
// If the message already visited the next node, doesn't send and returns false. Otherwise, return true.
func (p *KeySwitchingProtocol) sendToNext(msg interface{}) {
	err := p.SendTo(p.nextNodeInCircuit, msg)
	if err != nil {
		dbg.Lvl1("Had an error sending a message: ", err)
	}
}
