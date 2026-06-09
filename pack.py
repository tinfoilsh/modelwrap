import os
import sys
import subprocess
import uuid
import shutil
from hashlib import sha256
from huggingface_hub import snapshot_download, HfApi

VERITY_FORMAT = "1"
VERITY_HASH = "sha256"
VERITY_DATA_BLOCK_SIZE = "4096"
VERITY_HASH_BLOCK_SIZE = "4096"

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


cache_dir = os.getenv("CACHE_DIR") or "cache"
output_dir = os.getenv("OUTPUT_DIR") or "output"
hf_token = os.getenv("HF_TOKEN")
verify_after_pack = os.getenv("VERIFY") == "1"

args = sys.argv[1:]
if "--verify" in args:
    verify_after_pack = True
    args.remove("--verify")

model = os.getenv("MODEL")
if not model:
    if len(args) >= 1:
        model = args[0]
    else:
        raise Exception("MODEL environment variable not set")

if not os.path.exists(cache_dir):
    os.makedirs(cache_dir)
if not os.path.exists(output_dir):
    os.makedirs(output_dir)

if "@" not in model:
    api = HfApi(token=hf_token)
    info = api.model_info(model)
    if not info.sha:
        raise Exception(f"Could not resolve HEAD commit for {model}. Please specify commit explicitly: MODEL={model}@<commit>")
    print(f"Resolved {model} default branch HEAD -> {info.sha}")
    model = f"{model}@{info.sha}"

model_name, model_commit = model.split("@")
model_dir = os.path.join(cache_dir, model.replace("@", "/") )

print(f"Downloading {model} to {model_dir}")

snapshot_download(
    model_name, 
    local_dir=model_dir,
    token=hf_token,
    revision=model_commit,
)

# Remove cache dir for reproducibility
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
    print(f.read())
