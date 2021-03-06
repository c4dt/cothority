package ch.epfl.dedis.calypso;

import ch.epfl.dedis.byzcoin.ByzCoinRPC;
import ch.epfl.dedis.byzcoin.Instance;
import ch.epfl.dedis.byzcoin.InstanceId;
import ch.epfl.dedis.byzcoin.contracts.ChainConfigData;
import ch.epfl.dedis.lib.crypto.*;
import ch.epfl.dedis.lib.darc.DarcId;
import ch.epfl.dedis.lib.exception.CothorityCommunicationException;
import ch.epfl.dedis.lib.exception.CothorityCryptoException;
import ch.epfl.dedis.lib.exception.CothorityException;
import ch.epfl.dedis.lib.exception.CothorityNotFoundException;
import ch.epfl.dedis.lib.proto.Calypso;
import com.google.protobuf.ByteString;
import com.google.protobuf.InvalidProtocolBufferException;
import org.bouncycastle.crypto.Xof;
import org.bouncycastle.crypto.digests.SHAKEDigest;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;

/**
 * A WriteData is the data that is sent to the calypsoWrite contract store a write request with the encrypted document.
 * Stored on BzyCoin, it will have the following fields:
 * <p>
 * - EncData - the encrypted data, should be smaller than 8MB
 * - U, Ubar, E, F, Cs - the symmetric key used to encrypt the data, itself encrypted to the Long Term Secret key
 * - ExtraData - plain text data that is stored as-is on the ledger
 * - LTSID - the Long Term Secret ID used to encrypt the data
 */
public class WriteData {
    private Calypso.Write write;

    /**
     * Create a new document by giving all possible parameters. This call
     * supposes that the data sent here is already encrypted using the
     * keyMaterial and can be decrypted using keyMaterial.
     *
     * @param lts         Long Term Secret parameters
     * @param dataEnc     The ciphertext which will be stored _as is_ on ByzCoin. There is a limit on the size of the
     *                    ciphertext which depends on the block size of ByzCoin. Typically the limit is slightly below
     *                    the block size because the block also contains other metadata. You can find the block size
     *                    using ChainConfigInstance.
     * @param keyMaterial The symmetric key plus eventually an IV. This will be encrypted under the shared symmetricKey
     *                    of the cothority.
     * @param extraData   data that will _not be encrypted_ but will be visible in cleartext on ByzCoin.
     * @param publisher   The darc with a rule for calypsoWrite and calypsoRead.
     * @throws CothorityException if something went wrong
     */
    public WriteData(CreateLTSReply lts, byte[] dataEnc, byte[] keyMaterial, byte[] extraData, DarcId publisher) throws CothorityException {
        if (dataEnc.length > ChainConfigData.blocksizeMax) {
            throw new CothorityException("data length too long");
        }
        Calypso.Write.Builder wr = Calypso.Write.newBuilder();
        wr.setData(ByteString.copyFrom(dataEnc));
        if (extraData != null) {
            wr.setExtradata(ByteString.copyFrom(extraData));
        }
        wr.setLtsid(lts.getLTSID().toProto());
        encryptKey(wr, lts, keyMaterial, publisher);
        write = wr.build();
    }

    /**
     * Private constructor if we know Calypso.Write
     *
     * @param w Calypso.Write
     */
    private WriteData(Calypso.Write w) {
        write = w;
    }

    /**
     * Recreates a WriteData from an instanceid.
     *
     * @param bc a running Byzcoin service
     * @param id an instanceId of a WriteInstance
     * @return the new WriteData
     * @throws CothorityNotFoundException      if the requested instance cannot be found
     * @throws CothorityCommunicationException if something went wrong with the communication
     * @throws CothorityCryptoException        if there is something wrong with the proof
     */
    public static WriteData fromByzcoin(ByzCoinRPC bc, InstanceId id) throws CothorityNotFoundException, CothorityCommunicationException, CothorityCryptoException {
        return WriteData.fromInstance(Instance.fromByzcoin(bc, id));
    }

    /**
     * Recreates a WriteData from an instance.
     *
     * @param inst an instance representing a WriteData
     * @return WriteData
     * @throws CothorityNotFoundException if the requested instance cannot be found
     */
    public static WriteData fromInstance(Instance inst) throws CothorityNotFoundException {
        if (!inst.getContractId().equals(WriteInstance.ContractId)) {
            throw new CothorityNotFoundException("Wrong contract in instance");
        }
        try {
            return new WriteData(Calypso.Write.parseFrom(inst.getData()));
        } catch (InvalidProtocolBufferException e) {
            throw new CothorityNotFoundException("couldn't parse protobuffer for writeData: " + e.getMessage());
        }
    }

    /**
     * Encrypts the key material and stores it in the given Write.Builder.
     *
     * @param wr          the Write.Builder where the encrypted key will be stored
     * @param lts         the Long Term Secret to use
     * @param keyMaterial what should be threshold encrypted in the blockchain, it must be 28 bytes,
     *                    see Encryption.java for details.
     * @throws CothorityCryptoException if there's a problem with the cryptography
     */
    private void encryptKey(Calypso.Write.Builder wr, CreateLTSReply lts, byte[] keyMaterial, DarcId darcBaseID) throws CothorityCryptoException {
        if (keyMaterial.length != Encryption.KEYMATERIAL_LEN) {
            throw new CothorityCryptoException("invalid keyMaterial length, got " + keyMaterial.length + " but it must be " + Encryption.KEYMATERIAL_LEN);
        }
        try {
            Ed25519Pair randkp = new Ed25519Pair();
            Scalar r = randkp.scalar;
            Point U = randkp.point;
            wr.setU(U.toProto());

            Point C = lts.getX().mul(r);
            C = C.add(Ed25519Point.embed(keyMaterial));
            wr.setC(C.toProto());

            Point gBar = Ed25519Point.embed(lts.getLTSID().getId(), getXof(lts.getLTSID().getId()));
            Point Ubar = gBar.mul(r);
            wr.setUbar(Ubar.toProto());
            Ed25519Pair skp = new Ed25519Pair();
            Scalar s = skp.scalar;
            Point w = skp.point;
            Point wBar = gBar.mul(s);

            MessageDigest hash = MessageDigest.getInstance("SHA-256");
            hash.update(C.toBytes());
            hash.update(U.toBytes());
            hash.update(Ubar.toBytes());
            hash.update(w.toBytes());
            hash.update(wBar.toBytes());
            hash.update(darcBaseID.getId());
            Scalar E = new Ed25519Scalar(hash.digest());
            wr.setE(E.toProto());
            Scalar F = s.add(E.mul(r));
            wr.setF(F.toProto());
        } catch (NoSuchAlgorithmException e) {
            throw new RuntimeException("Hashing-error: " + e.getMessage());
        }
    }

    // Used for when we need to embed the point deterministically with a seed.
    private static Xof getXof(byte[] seed) {
        SHAKEDigest d = new SHAKEDigest(256);
        d.update(seed, 0, seed.length);
        return d;
    }

    /**
     * Get the encrypted data.
     *
     * @return the encrypted data
     */
    public byte[] getDataEnc() {
        return write.getData().toByteArray();
    }

    /**
     * Get the extra data.
     *
     * @return the extra data
     */
    public byte[] getExtraData() {
        return write.getExtradata().toByteArray();
    }

    public Calypso.Write toProto() {
        return write;
    }

    /**
     * Takes a byte array as an input to parse into the protobuf representation of WriteData.
     *
     * @param buf the protobuf data
     * @return WriteData
     * @throws InvalidProtocolBufferException if the protobuf data is invalid.
     */
    public static WriteData fromProto(byte[] buf) throws InvalidProtocolBufferException {
        return new WriteData(Calypso.Write.parseFrom(buf));
    }
}
