# Modelwrap Specification

Modelwrap (MWP) is a verified model artifact format for pinning model weights during inference. Encrypted Modelwrap (EMWP) is an encrypted encoding of the same plaintext layout that provides confidentiality for model weights at rest and in distribution.

The trust assumptions are: artifact bytes are *untrusted*; at runtime, identity and verification parameters come from an *attested* config; and EMWP decryption keys come from an external secret path.

Modelwrap's verifiability is based on *reproducibility*, not attested provenance: artifacts are bit-for-bit reproducible, and the Modelwrap root hash is derived deterministically. An auditor can independently recompute the root hash from the model weights, for example from a pinned Hugging Face revision.

## Reproducibility and Packer Versioning

Artifact bytes, and therefore root hashes, are reproducible with respect to a specific digest-pinned packer image: the EROFS encoder and dm-verity tooling contribute to the output bytes, so a toolchain upgrade (e.g. a new `erofs-utils` version) can produce a different root hash for the same input model. This is not a format change and requires no version field in the artifact: existing artifacts remain valid and mountable because consumers act only on the attested reference, never on tool versions. An auditor recomputing a root hash must use the same packer image digest that produced the artifact.

## MWP Format

An MWP format is:

```text
[ deterministic EROFS filesystem ][ dm-verity hash tree ]
```

The EROFS filesystem contains the model files. The dm-verity tree authenticates the EROFS bytes.

The compact MWP reference is:

```text
<rootHash>_<hashOffset>_<uuid>
```

Where:

- `rootHash` is the dm-verity root hash.
- `hashOffset` is the byte offset where the dm-verity hash tree starts.
- `uuid` locates the plaintext MWP disk at `/dev/disk/by-uuid/<uuid>`.

Example use in an attested tinfoil config:

```yaml
models:
  - name: public-model
    repo: "<hf_org>@<revision>"
    mwp: <rootHash>_<hashOffset>_<uuid>
```

> `mpk` is accepted as a legacy alias for plaintext `mwp`.

## MWP Mounting

At boot, the secure enclave does not mount the MWP disk directly. It first asks dm-verity to create a verified read-only mapper device using the trusted `rootHash` and `hashOffset`. It then mounts that verified mapper as EROFS.

### dm-verity Parameter Pinning

The verity mapping is always opened with `--no-superblock` and fully explicit parameters, so neither veritysetup nor the kernel ever parses on-disk metadata:

- Hash algorithm, format version, and block sizes are fixed format constants.
- The data size is `hashOffset / 4096` blocks, derived from the *attested* reference: the MWP data area is exactly the bytes preceding the hash area. This forecloses tampering with the on-disk data-size field to truncate the mapped device.
- The hash tree starts at `hashOffset + 4096` (one hash block past the superblock slot).
- The salt is `SHA256(<model identity>)` where the model identity is the packer's `name@revision` string, carried in the attested config as the required `repo` field. The consumer re-derives it; a wrong `repo` fails closed because nothing verifies against the attested root hash.

The artifact still contains a dm-verity superblock at `hashOffset` (the packer writes one at format time), but consumers never read it: it is untrusted dead bytes occupying the first hash block.

The mount path is:

```text
/tinfoil/mwp/mwp-<rootHash>
```
> `/tinfoil/mpk/mpk-<rootHash>` is accepted as a legacy alias.

If any model mount step fails, boot fails closed.

## EMWP Extension

EMWP adds confidentiality by encrypting the MWP plaintext artifact.

An EMWP artifact is:

```text
GPT disk image
  -> encrypted payload partition (located by PARTUUID)
  -> raw dm-crypt encryption of:
     [ deterministic EROFS filesystem ][ dm-verity hash tree ]
```

After decryption, the bytes are exactly the MWP plaintext layout. dm-verity is still applied to the decrypted bytes, not to the ciphertext.

Example config:

```yaml
models:
  - name: private-model
    repo: "<hf_org>@<revision>"
    emwp: <rootHash>_<hashOffset>_<uuid>
    key-secret: PRIVATE_MODEL_KEY
```

## EMWP Reference Subtlety

MWP and EMWP use the same compact reference syntax, but the `uuid` locates different things:

- For MWP, `uuid` locates the plaintext MWP disk.
- For EMWP, `uuid` is the GPT partition PARTUUID for the encrypted payload partition.

In both cases, `rootHash` and `hashOffset` describe the plaintext verified layout. They do not authenticate the ciphertext directly.

## EMWP Encryption

EMWP uses raw dm-crypt:

```text
cipher:      aes-xts-plain64
key size:    512 bits
sector size: 4096 bytes
```

The encrypted payload does not contain trusted encryption headers. No LUKS metadata is trusted or required as the encryption parameters are fixed. The outer GPT partition table is only a locator for `/dev/disk/by-partuuid/<uuid>`; it is not trusted for model integrity.

## EMWP Key Derivation

The external secret named by `key-secret` is a base64-encoded 64-byte EMWP master key.

This master key is not used directly as the dm-crypt volume key. The raw 64-byte dm-crypt key is derived with HKDF-SHA256:

```text
IKM:  decoded 64-byte EMWP master key
salt: <rootHash>_<uuid>
info: tinfoil/emwp/dm-crypt-key/v1
L:    64 bytes
```

This binds the raw dm-crypt key to the attested artifact identity and avoids reusing the same raw XTS key across artifacts.

Packers and consumer must use the same derivation.

## EMWP Mounting

The CVM mounts EMWP as:

```text
encrypted disk
  -> dm-crypt(derived key)
  -> decrypted MWP plaintext layout
  -> dm-verity(rootHash, hashOffset)
  -> read-only EROFS mount
```

The EROFS mount uses:

```text
ro,nodev,nosuid,noexec
```

If any model mount step fails, boot fails closed.

## Trust Assumptions

Trusted:

- The CVM image and boot code.
- The attested `cvmimage` config.
- The model artifact contents.
- The external secret delivery path.

Untrusted:

- The MWP or EMWP block device bytes.
- The storage or distribution path carrying the artifact.
- Any mutable metadata inside the artifact.
- The party that packs the modelwrap.

Important consequences:

- dm-verity integrity depends on the `rootHash` from *attested* config.
- No artifact bytes are ever parsed before verification: every verity parameter is a pinned constant or derived from attested values (`rootHash`, `hashOffset`, `repo`), never read from the artifact.
- EMWP confidentiality depends on the external EMWP master key.
- EMWP authenticity is still the plaintext dm-verity check after decryption.
- A malicious but correctly attested model artifact can still contain malicious model code or data.

## Security Properties

MWP provides:

- Integrity of model bytes through dm-verity and the attested root hash.
- Reproducible model identity through deterministic EROFS and verity parameters.

EMWP additionally provides:

- Confidentiality of model bytes against anyone without the external EMWP master key.
- Per-artifact raw dm-crypt keys derived from a stable master key.
- No trust in mutable encryption headers or metadata inside the artifact.

## Non-Goals

MWP/EMWP do not:

- Hide model size or coarse layout.
- Make malicious model contents safe to execute.
- Provide rollback protection by themselves.
- Replace the need to trust the config attestation path.
- Authenticate EMWP ciphertext before decryption.
