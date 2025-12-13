import os
import sys
import subprocess
import uuid
import shutil
from hashlib import sha256
from huggingface_hub import snapshot_download, HfApi

cache_dir = os.getenv("CACHE_DIR") or "cache"
output_dir = os.getenv("OUTPUT_DIR") or "output"

model = os.getenv("MODEL")
if not model:
    if len(sys.argv) >= 2:
        model = sys.argv[1]
    else:
        raise Exception("MODEL environment variable not set")

if not os.path.exists(cache_dir):
    os.makedirs(cache_dir)
if not os.path.exists(output_dir):
    os.makedirs(output_dir)

if "@" not in model:
    api = HfApi()
    refs = api.list_repo_refs(model)
    latest_commit = refs.branches[0].target_commit
    print(f"Found latest commit for {model} -> {latest_commit}")
    model = f"{model}@{latest_commit}"

model_name, model_commit = model.split("@")
model_dir = os.path.join(cache_dir, model.replace("@", "/") )

print(f"Downloading {model} to {model_dir}")

snapshot_download(
    model_name, 
    local_dir=model_dir,
    token=os.getenv("HF_TOKEN"),
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

with open(info_file, "r") as f:
    print(f.read())
