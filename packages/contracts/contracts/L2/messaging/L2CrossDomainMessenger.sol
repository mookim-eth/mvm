// SPDX-License-Identifier: MIT
pragma solidity ^0.8.9;

/* Library Imports */
import { AddressAliasHelper } from "../../standards/AddressAliasHelper.sol";
import { Lib_CrossDomainUtils } from "../../libraries/bridge/Lib_CrossDomainUtils.sol";
import { Lib_DefaultValues } from "../../libraries/constants/Lib_DefaultValues.sol";
import { Lib_PredeployAddresses } from "../../libraries/constants/Lib_PredeployAddresses.sol";

/* Interface Imports */
import { IL2CrossDomainMessenger } from "./IL2CrossDomainMessenger.sol";
import { iOVM_L2ToL1MessagePasser } from "../predeploys/iOVM_L2ToL1MessagePasser.sol";
import { OVM_DeployerWhitelist } from "../predeploys/OVM_DeployerWhitelist.sol";

/**
 * @title L2CrossDomainMessenger
 * @dev The L2 Cross Domain Messenger contract sends messages from L2 to L1, and is the entry point
 * for L2 messages sent via the L1 Cross Domain Messenger.
 *
 */
contract L2CrossDomainMessenger is IL2CrossDomainMessenger {
    /*************
     * Variables *
     *************/

    mapping(bytes32 => bool) public relayedMessages;
    mapping(bytes32 => bool) public successfulMessages;
    mapping(bytes32 => bool) public sentMessages;
    uint256 public messageNonce;
    address internal xDomainMsgSender = Lib_DefaultValues.DEFAULT_XDOMAIN_SENDER;
    address public l1CrossDomainMessenger;

    /***************
     * Constructor *
     ***************/

    constructor(address _l1CrossDomainMessenger) {
        l1CrossDomainMessenger = _l1CrossDomainMessenger;
    }

    /**********************
     * Function Modifiers *
     **********************/

    modifier onlyWhitelisted() {
        require(
            OVM_DeployerWhitelist(Lib_PredeployAddresses.DEPLOYER_WHITELIST).isXDomainSenderAllowed(
                    msg.sender
                ),
            // solhint-disable-next-line max-line-length
            "L2 to L1 messages are restricted to whitelisted senders."
        );
        _;
    }

    /********************
     * Public Functions *
     ********************/

    function xDomainMessageSender() public view returns (address) {
        require(
            xDomainMsgSender != Lib_DefaultValues.DEFAULT_XDOMAIN_SENDER,
            "xDomainMessageSender is not set"
        );
        return xDomainMsgSender;
    }

    /**
     * Sends a cross domain message to the target messenger.
     * @param _target Target contract address.
     * @param _message Message to send to the target.
     * @param _gasLimit Gas limit for the provided message.
     */
    function sendMessage(
        address _target,
        bytes memory _message,
        uint32 _gasLimit
    ) public payable onlyWhitelisted {
        bytes memory xDomainCalldata = Lib_CrossDomainUtils.encodeXDomainCalldata(
            _target,
            msg.sender,
            _message,
            messageNonce
        );

        sentMessages[keccak256(xDomainCalldata)] = true;

        (bool sent, ) = Lib_PredeployAddresses.SEQUENCER_FEE_WALLET.call{ value: msg.value }("");
        require(sent, "Failed to send bridge fee");

        // Actually send the message.
        iOVM_L2ToL1MessagePasser(Lib_PredeployAddresses.L2_TO_L1_MESSAGE_PASSER).passMessageToL1(
            xDomainCalldata
        );

        // Emit an event before we bump the nonce or the nonce will be off by one.
        emit SentMessage(_target, msg.sender, _message, messageNonce, _gasLimit, 1); //sending to l1 so chainid set to 1
        messageNonce += 1;
    }

    /**
     * Relays a cross domain message to a contract.
     * @inheritdoc IL2CrossDomainMessenger
     */
    function relayMessage(
        address _target,
        address _sender,
        bytes memory _message,
        uint256 _messageNonce
    ) public {
        require(
            AddressAliasHelper.undoL1ToL2Alias(msg.sender) == l1CrossDomainMessenger,
            "Provided message could not be verified."
        );

        bytes memory xDomainCalldata = Lib_CrossDomainUtils.encodeXDomainCalldata(
            _target,
            _sender,
            _message,
            _messageNonce
        );

        bytes32 xDomainCalldataHash = keccak256(xDomainCalldata);

        require(
            successfulMessages[xDomainCalldataHash] == false,
            "Provided message has already been received."
        );

        // Prevent calls to OVM_L2ToL1MessagePasser, which would enable
        // an attacker to maliciously craft the _message to spoof
        // a call from any L2 account.
        if (_target == Lib_PredeployAddresses.L2_TO_L1_MESSAGE_PASSER) {
            // Write to the successfulMessages mapping and return immediately.
            successfulMessages[xDomainCalldataHash] = true;
            return;
        }

        xDomainMsgSender = _sender;
        (bool success, ) = _target.call(_message);
        xDomainMsgSender = Lib_DefaultValues.DEFAULT_XDOMAIN_SENDER;

        // Mark the message as received if the call was successful. Ensures that a message can be
        // relayed multiple times in the case that the call reverted.
        if (success == true) {
            successfulMessages[xDomainCalldataHash] = true;
            emit RelayedMessage(xDomainCalldataHash);
        } else {
            emit FailedRelayedMessage(xDomainCalldataHash);
        }

        // Store an identifier that can be used to prove that the given message was relayed by some
        // user. Gives us an easy way to pay relayers for their work.
        bytes32 relayId = keccak256(abi.encodePacked(xDomainCalldata, msg.sender, block.number));

        relayedMessages[relayId] = true;
    }

    function sendMessageViaChainId(
        uint256 _chainId,
        address _target,
        bytes calldata _message,
        uint32 _gasLimit
    ) public payable {}
}
