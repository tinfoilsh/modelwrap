import os
import argparse
import base64
import hashlib
import hmac
import subprocess
import uuid
import shutil
from hashlib import sha256
from huggingface_hub import snapshot_download, HfApi

VERITY_FORMAT = "1"
VERITY_HASH = "sha256"
VERITY_DATA_BLOCK_SIZE = "4096"
VERITY_HASH_BLOCK_SIZE = "4096"
EMWP_CIPHER = "aes-xts-plain64"
EMWP_KEY_SIZE = "512"
EMWP_SECTOR_SIZE = "4096"
EMWP_KEY_DERIVE_INFO = b"tinfoil/emwp/dm-crypt-key/v1"
GPT_SECTOR_SIZE = 512
EMWP_SECTOR_SIZE_BYTES = 4096
EMWP_PARTITION_START_SECTOR = 2048
EMWP_GPT_TRAILING_SECTORS = 40


def hkdf_sha256(ikm, salt, info, length):
    prk = hmac.new(salt, ikm, hashlib.sha256).digest()
    okm = b""
    previous = b""
    counter = 1
    while len(okm) < length:
        previous = hmac.new(prk, previous + info + bytes([counter]), hashlib.sha256).digest()
        okm += previous
        counter += 1
    return okm[:length]


def hash_model_dir(model_dir):
    digest = hashlib.sha256()
    for root, dirs, files in os.walk(model_dir):
        dirs.sort()
        files.sort()
        rel_root = os.path.relpath(root, model_dir)
        if rel_root != ".":
            digest.update(b"d\0")
            digest.update(rel_root.encode())
            digest.update(b"\0")
        for name in files:
            path = os.path.join(root, name)
            rel_path = os.path.relpath(path, model_dir)
            if os.path.islink(path):
                digest.update(b"l\0")
                digest.update(rel_path.encode())
                digest.update(b"\0")
                digest.update(os.readlink(path).encode())
                digest.update(b"\0")
                continue
            digest.update(b"f\0")
            digest.update(rel_path.encode())
            digest.update(b"\0")
            with open(path, "rb") as f:
                for chunk in iter(lambda: f.read(1024 * 1024), b""):
                    digest.update(chunk)
            digest.update(b"\0")
    return digest.hexdigest()


def emwp_master_key(key_file=None):
    if not key_file:
        key_file = os.getenv("PRIVATE_MODEL_KEY_FILE")
    if key_file:
        with open(key_file, "r") as f:
            key_b64 = f.read().strip()
    else:
        key_b64 = os.getenv("PRIVATE_MODEL_KEY_B64")
    if not key_b64:
        raise Exception("--key-file, PRIVATE_MODEL_KEY_FILE, or PRIVATE_MODEL_KEY_B64 is required for --encrypt")
    key = base64.b64decode(key_b64, validate=True)
    if len(key) != 64:
        raise Exception(f"EMWP master key decoded to {len(key)} bytes, want 64")
    return key


def write_key_file(path, key):
    with open(path, "wb") as f:
        f.write(key)
    os.chmod(path, 0o600)


def close_crypt_mapper(name):
    subprocess.run(["cryptsetup", "close", name], check=False)


def encrypt_emwp(mwp_file, emwp_file, root_hash, part_uuid, key_file=None):
    size = os.path.getsize(mwp_file)
    encrypted_size = (size + EMWP_SECTOR_SIZE_BYTES - 1) // EMWP_SECTOR_SIZE_BYTES * EMWP_SECTOR_SIZE_BYTES
    sectors = encrypted_size // GPT_SECTOR_SIZE
    end_sector = EMWP_PARTITION_START_SECTOR + sectors - 1
    total_sectors = end_sector + 1 + EMWP_GPT_TRAILING_SECTORS
    disk_uuid = str(uuid.uuid5(uuid.NAMESPACE_URL, root_hash + "-emwp-disk"))
    tmp_file = emwp_file + ".tmp"
    dm_key_file = emwp_file + ".key.tmp"
    mapper_name = "modelwrap-emwp-" + root_hash[:16]

    master_key = emwp_master_key(key_file)
    dm_key = hkdf_sha256(
        master_key,
        f"{root_hash}_{part_uuid}".encode(),
        EMWP_KEY_DERIVE_INFO,
        64,
    )

    for path in (tmp_file, dm_key_file):
        if os.path.exists(path):
            os.remove(path)

    try:
        print(f"Creating EMWP GPT image {emwp_file}")
        subprocess.run(["truncate", "-s", str(total_sectors * GPT_SECTOR_SIZE), tmp_file], check=True)
        subprocess.run([
            "sgdisk",
            "--clear",
            f"--disk-guid={disk_uuid}",
            f"--new=1:{EMWP_PARTITION_START_SECTOR}:{end_sector}",
            "--typecode=1:8300",
            f"--partition-guid=1:{part_uuid}",
            "--change-name=1:emwp",
            tmp_file,
        ], check=True)

        write_key_file(dm_key_file, dm_key)
        subprocess.run([
            "cryptsetup", "open",
            "--type", "plain",
            "--cipher", EMWP_CIPHER,
            "--key-size", EMWP_KEY_SIZE,
            "--sector-size", EMWP_SECTOR_SIZE,
            "--key-file", dm_key_file,
            "--offset", str(EMWP_PARTITION_START_SECTOR),
            "--skip", "0",
            "--size", str(sectors),
            tmp_file,
            mapper_name,
        ], check=True)

        subprocess.run([
            "dd",
            f"if={mwp_file}",
            f"of=/dev/mapper/{mapper_name}",
            "bs=4M",
            "conv=fsync",
            "status=none",
        ], check=True)
        close_crypt_mapper(mapper_name)
        os.replace(tmp_file, emwp_file)
    finally:
        close_crypt_mapper(mapper_name)
        if os.path.exists(dm_key_file):
            os.remove(dm_key_file)
        if os.path.exists(tmp_file):
            os.remove(tmp_file)


def verify_emwp(emwp_file, info_file, key_file_override=None):
    raw_info, root_hash, offset, part_uuid = parse_info_file(info_file)
    if not os.path.exists(emwp_file):
        raise Exception(f"EMWP artifact not found: {emwp_file}")

    size = os.path.getsize(emwp_file)
    sectors = size // GPT_SECTOR_SIZE - EMWP_PARTITION_START_SECTOR - EMWP_GPT_TRAILING_SECTORS
    key_file = emwp_file + ".key.tmp"
    mapper_name = "modelwrap-emwp-verify-" + root_hash[:16]
    dm_key = hkdf_sha256(
        emwp_master_key(key_file_override),
        f"{root_hash}_{part_uuid}".encode(),
        EMWP_KEY_DERIVE_INFO,
        64,
    )

    try:
        write_key_file(key_file, dm_key)
        subprocess.run([
            "cryptsetup", "open",
            "--type", "plain",
            "--cipher", EMWP_CIPHER,
            "--key-size", EMWP_KEY_SIZE,
            "--sector-size", EMWP_SECTOR_SIZE,
            "--key-file", key_file,
            "--offset", str(EMWP_PARTITION_START_SECTOR),
            "--skip", "0",
            "--size", str(sectors),
            emwp_file,
            mapper_name,
        ], check=True)

        verify_verity(f"/dev/mapper/{mapper_name}", info_file)
    finally:
        close_crypt_mapper(mapper_name)
        if os.path.exists(key_file):
            os.remove(key_file)
    return raw_info

def parse_info_file(info_file):
    with open(info_file, "r") as f:
        raw_info = f.read().strip()

    parts = raw_info.split("_")
    if len(parts) != 3:
        raise Exception(
            f"Invalid info file {info_file}: expected ROOTHASH_OFFSET_VERITYUUID"
        )

    root_hash, offset, verity_uuid = parts
    if len(root_hash) != 64 or any(c not in "0123456789abcdef" for c in root_hash):
        raise Exception(f"Invalid root hash in {info_file}: {root_hash}")
    if not offset.isdigit():
        raise Exception(f"Invalid hash offset in {info_file}: {offset}")
    try:
        uuid.UUID(verity_uuid)
    except ValueError as err:
        raise Exception(f"Invalid verity UUID in {info_file}: {verity_uuid}") from err

    return raw_info, root_hash, offset, verity_uuid


def verify_verity(mpk_file, info_file):
    raw_info, root_hash, offset, _ = parse_info_file(info_file)
    if not os.path.exists(mpk_file):
        raise Exception(f"MWP artifact not found: {mpk_file}")

    verify_cmd = [
        "veritysetup",
        f"--format={VERITY_FORMAT}",
        f"--hash={VERITY_HASH}",
        f"--data-block-size={VERITY_DATA_BLOCK_SIZE}",
        f"--hash-block-size={VERITY_HASH_BLOCK_SIZE}",
        f"--hash-offset={offset}",
        "verify",
        mpk_file,  # data dev
        mpk_file,  # hash dev
        root_hash,
    ]
    print(f"Verifying dm-verity artifact {mpk_file}")
    subprocess.run(verify_cmd, check=True)
    print("Verification OK.")
    return raw_info


parser = argparse.ArgumentParser()
parser.add_argument("model", nargs="?", default=os.getenv("MODEL"))
parser.add_argument("--verify", action="store_true", default=os.getenv("VERIFY") == "1")
parser.add_argument("--encrypt", action="store_true", default=os.getenv("ENCRYPTION") == "1")
parser.add_argument("--model-dir", default=os.getenv("MODEL_DIR"))
parser.add_argument("--key-file")
args = parser.parse_args()

cache_dir = os.getenv("CACHE_DIR") or "cache"
output_dir = os.getenv("OUTPUT_DIR") or "output"
model_dir_override = args.model_dir
hf_token = os.getenv("HF_TOKEN")
verify_after_pack = args.verify
encrypt_output = args.encrypt
model = args.model

if not os.path.exists(cache_dir):
    os.makedirs(cache_dir)
if not os.path.exists(output_dir):
    os.makedirs(output_dir)

if model_dir_override:
    if not os.path.isdir(model_dir_override):
        raise Exception(f"MODEL_DIR is not a directory: {model_dir_override}")
    local_revision = hash_model_dir(model_dir_override)
    if not model:
        local_name = os.path.basename(os.path.abspath(model_dir_override)) or "model"
        model = f"{local_name}@{local_revision}"
    elif "@" not in model:
        model = f"{model}@{local_revision}"
elif not model:
    raise Exception("model argument or MODEL environment variable is required")

if not model_dir_override and "@" not in model:
    api = HfApi(token=hf_token)
    info = api.model_info(model)
    if not info.sha:
        raise Exception(f"Could not resolve HEAD commit for {model}. Please specify commit explicitly: MODEL={model}@<commit>")
    print(f"Resolved {model} default branch HEAD -> {info.sha}")
    model = f"{model}@{info.sha}"

model_name, model_commit = model.split("@", 1)
model_dir = model_dir_override or os.path.join(cache_dir, model.replace("@", "/") )

if model_dir_override:
    print(f"Using local model directory {model_dir} as {model}")
else:
    print(f"Downloading {model} to {model_dir}")

    snapshot_download(
        model_name,
        local_dir=model_dir,
        token=hf_token,
        revision=model_commit,
    )

# Remove cache dir for reproducibility
if not model_dir_override:
    cache_dir = os.path.join(model_dir, ".cache")
    if os.path.exists(cache_dir):
        shutil.rmtree(cache_dir)

# Create EROFS image

output_model_dir = os.path.join(output_dir, model_name)
if not os.path.exists(output_model_dir):
    os.makedirs(output_model_dir)

mpk_file = os.path.join(output_model_dir, f"{model_commit}.mpk")

if not os.path.exists(mpk_file):
    mkfs_cmd = [
        "mkfs.erofs",
        "--all-root",
        "-T0", # Zero timestamp
        f"-U{uuid.uuid5(uuid.NAMESPACE_URL, model+'-inner')}", # Static UUID
        mpk_file+".tmp",
        model_dir
    ]
    print(f"Creating EROFS image {mpk_file}")
    subprocess.run(mkfs_cmd, check=True)
    os.rename(mpk_file+".tmp", mpk_file)
else:
    print(f"Using existing EROFS image {mpk_file}")

# Wrap with dm-verity

info_file = os.path.join(output_dir, model_name, f"{model_commit}.info")

if not os.path.exists(info_file):
    size = os.path.getsize(mpk_file)
    offset = (size + 4095) // 4096 * 4096

    verity_uuid = uuid.uuid5(uuid.NAMESPACE_URL, model+'-inner')

    veritysetup_cmd = [
        "veritysetup",
        f"--format={VERITY_FORMAT}",
        f"--hash={VERITY_HASH}",
        f"--data-block-size={VERITY_DATA_BLOCK_SIZE}",
        f"--hash-block-size={VERITY_HASH_BLOCK_SIZE}",
        f"--salt={sha256(model.encode()).hexdigest()}",
        f"--uuid={verity_uuid}",
        f"--hash-offset={offset}",
        f"--root-hash-file={info_file}",
        "format",
        mpk_file, # data dev
        mpk_file, # hash dev
    ]
    print(f"Running veritysetup on {mpk_file}")
    subprocess.run(veritysetup_cmd, check=True)

    if not os.path.exists(info_file):
        raise Exception(f"Failed to create dm-verity info file {info_file}")

    with open(info_file, "a") as f:
        f.write(f"_{offset}_{verity_uuid}")
else:
    print(f"dm-verity volume already exists at {mpk_file}")

if verify_after_pack:
    verify_verity(mpk_file, info_file)
else:
    print("Skipping dm-verity verification. Pass --verify or set VERIFY=1 to verify cached artifacts.")

with open(info_file, "r") as f:
    info = f.read().strip()

if encrypt_output:
    _, root_hash, offset, _ = parse_info_file(info_file)
    part_uuid = str(uuid.uuid5(uuid.NAMESPACE_URL, model + "-emwp-outer"))
    emwp_file = os.path.join(output_model_dir, f"{model_commit}.emwp")
    emwp_info_file = os.path.join(output_model_dir, f"{model_commit}.emwp.info")
    emwp_info = f"{root_hash}_{offset}_{part_uuid}"

    if not os.path.exists(emwp_file):
        encrypt_emwp(mpk_file, emwp_file, root_hash, part_uuid, args.key_file)
    else:
        print(f"Using existing EMWP artifact: {emwp_file}")

    with open(emwp_info_file+".tmp", "w") as f:
        f.write(emwp_info)
    os.replace(emwp_info_file+".tmp", emwp_info_file)

    if verify_after_pack:
        verify_emwp(emwp_file, emwp_info_file, args.key_file)
    print(emwp_info)
else:
    print(info)
