#!/usr/bin/env bash
set -euo pipefail

ARCH="${1:-}"
if [[ -z "${ARCH}" ]]; then
  echo "usage: $0 <amd64|arm64>" >&2
  exit 2
fi

work_dir="$(pwd)"
bundle_dir="${work_dir}/bundle_${ARCH}"
dist_dir="${work_dir}/dist"
mkdir -p "${dist_dir}"

rm -rf "${bundle_dir}"
mkdir -p "${bundle_dir}"/{awscli,glibc}

build_host_amd64() {
  # 1) Fetch AWS CLI (amd64) and copy dist
  curl -fsSL -o /tmp/awscli.zip https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip
  rm -rf /tmp/awszip && mkdir -p /tmp/awszip
  unzip -q /tmp/awscli.zip -d /tmp/awszip
  cp -a /tmp/awszip/aws/dist "${bundle_dir}/awscli/"
  AWSBIN="${bundle_dir}/awscli/dist/aws"
  chmod 0755 "${AWSBIN}"

  # 2) Collect runtime libs required by aws and libpython*.so
  mapfile -t LIBS < <(
    {
      ldd "${AWSBIN}" || true
      PYLIB="$(ls "${bundle_dir}/awscli/dist"/libpython*.so* 2>/dev/null | head -n1 || true)"
      if [[ -n "${PYLIB:-}" && -f "${PYLIB}" ]]; then
        ldd "${PYLIB}" || true
      fi
    } | awk '/=>/ {print $3} !/=>/ {print $1}' | awk 'NF' | sort -u
  )

  add_by_name() {
    local name="$1" cand=""
    cand="$([ -x /sbin/ldconfig ] && /sbin/ldconfig -p 2>/dev/null | awk -v n="$name" '$1==n {print $NF; exit}')" || true
    [[ -z "$cand" ]] && cand="$(ldconfig -p 2>/dev/null | awk -v n="$name" '$1==n {print $NF; exit}')" || true
    [[ -z "$cand" ]] && cand="$(find /lib /usr/lib /usr/local/lib -name "$name" -type f 2>/dev/null | head -n1 || true)"
    [[ -n "$cand" ]] && echo "$cand"
  }

  EXTRA=()
  for n in libutil.so.1 libm.so.6 librt.so.1 libgcc_s.so.1 libstdc++.so.6; do
    cand="$(add_by_name "$n" || true)"
    [[ -n "$cand" ]] && EXTRA+=("$cand")
  done

  printf '%s\n' "${LIBS[@]}" "${EXTRA[@]}" | awk 'NF' | sort -u > /tmp/_libs.txt
  while IFS= read -r lib; do
    [[ "$lib" == "linux-vdso.so.1" ]] && continue
    [[ -e "$lib" ]] || continue
    cp -Lv "$lib" "${bundle_dir}/glibc/" || true
  done < /tmp/_libs.txt

  # 3) Include dynamic loader
  LOADER="$(ldd "${AWSBIN}" | awk '/ld-linux/ {print $1}' | head -n1 || true)"
  if [[ -z "${LOADER:-}" || ! -e "${LOADER}" ]]; then
    for cand in /lib64/ld-linux-x86-64.so.2 /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2; do
      if [[ -e "$cand" ]]; then LOADER="$cand"; break; fi
    done
  fi
  if [[ -z "${LOADER:-}" || ! -e "${LOADER}" ]]; then
    echo "Dynamic loader not found" >&2; exit 1
  fi
  cp -Lv "${LOADER}" "${bundle_dir}/glibc/"

  # 4) Fix permissions
  chmod 0755 "${bundle_dir}/awscli/dist/aws" || true
  for f in "${bundle_dir}/glibc/"*; do
    case "$(basename "$f")" in
      ld-linux*|*ld-*.so*) chmod 0755 "$f" ;;
      *)                    chmod 0644 "$f" ;;
    esac
  done

  # 5) Package
  tar -C "${bundle_dir}" -czf "${dist_dir}/aws_linux_amd64.tar.gz" awscli glibc
  echo "Created ${dist_dir}/aws_linux_amd64.tar.gz"
}

build_via_docker_arm64() {
  docker run --rm --platform linux/arm64 -v "${work_dir}:${work_dir}" -w "${work_dir}" ubuntu:22.04 bash -lc '
    set -euo pipefail
    apt-get update -y
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends curl unzip ca-certificates libc-bin findutils file

    BUNDLE_DIR="'"${bundle_dir}"'"
    rm -rf "$BUNDLE_DIR" && mkdir -p "$BUNDLE_DIR"/{awscli,glibc}

    curl -fsSL -o /tmp/awscli.zip https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip
    rm -rf /tmp/awszip && mkdir -p /tmp/awszip
    unzip -q /tmp/awscli.zip -d /tmp/awszip
    cp -a /tmp/awszip/aws/dist "$BUNDLE_DIR/awscli/"
    AWSBIN="$BUNDLE_DIR/awscli/dist/aws"
    chmod 0755 "$AWSBIN"

    mapfile -t LIBS < <(
      {
        ldd "$AWSBIN" || true
        PYLIB="$(ls "$BUNDLE_DIR/awscli/dist"/libpython*.so* 2>/dev/null | head -n1 || true)"
        if [[ -n "${PYLIB:-}" && -f "$PYLIB" ]]; then
          ldd "$PYLIB" || true
        fi
      } | awk "/=>/ {print \$3} !/=>/ {print \$1}" | awk "NF" | sort -u
    )

    add_by_name() {
      local name="$1" cand=""
      cand="$([ -x /sbin/ldconfig ] && /sbin/ldconfig -p 2>/dev/null | awk -v n="$name" "\$1==n {print \$NF; exit}")" || true
      [[ -z "$cand" ]] && cand="$(ldconfig -p 2>/dev/null | awk -v n="$name" "\$1==n {print \$NF; exit}")" || true
      [[ -z "$cand" ]] && cand="$(find /lib /usr/lib /usr/local/lib -name "$name" -type f 2>/dev/null | head -n1 || true)"
      [[ -n "$cand" ]] && echo "$cand"
    }

    EXTRA=()
    for n in libutil.so.1 libm.so.6 librt.so.1 libgcc_s.so.1 libstdc++.so.6; do
      cand="$(add_by_name "$n" || true)"
      [[ -n "$cand" ]] && EXTRA+=("$cand")
    done

    printf '%s\n' "${LIBS[@]}" "${EXTRA[@]}" | awk 'NF' | sort -u > /tmp/_libs_arm64.txt
    while IFS= read -r lib; do
      [[ "$lib" == "linux-vdso.so.1" ]] && continue
      [[ -e "$lib" ]] || continue
      cp -Lv "$lib" "$BUNDLE_DIR/glibc/" || true
    done < /tmp/_libs_arm64.txt

    LOADER="$(ldd "$AWSBIN" | awk "/ld-linux/ {print \$1}" | head -n1 || true)"
    if [[ -z "${LOADER:-}" || ! -e "$LOADER" ]]; then
      for cand in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-aarch64.so.1; do
        if [[ -e "$cand" ]]; then LOADER="$cand"; break; fi
      done
    fi
    if [[ -z "${LOADER:-}" || ! -e "$LOADER" ]]; then
      echo "Dynamic loader not found (arm64)" >&2; exit 1
    fi
    cp -Lv "$LOADER" "$BUNDLE_DIR/glibc/"
  '

  # Package on host
  tar -C "${bundle_dir}" -czf "${dist_dir}/aws_linux_arm64.tar.gz" awscli glibc
  echo "Created ${dist_dir}/aws_linux_arm64.tar.gz"
}

case "${ARCH}" in
  amd64) build_host_amd64 ;;
  arm64) build_via_docker_arm64 ;;
  *) echo "unsupported arch: ${ARCH}" >&2; exit 1 ;;
esac

