#!/usr/bin/env python3
"""Offline conformance checks for the extracted dsh-TUI whale and Mist assets.

Uses only the Python standard library and never opens a network connection.
Run from anywhere: python cmd/dshtui/assets/verify_assets.py
"""
from __future__ import annotations

import hashlib
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
COMMIT = "1bd7cb515b4c6fb52f049f7bde521df48ee2f54b"
FRAMES_JSON_SHA256 = "2c9b9e2a5f35a7d05596be68f41937059963ec2a76436c462f4e66de42426ca8"
THEME_JSON_SHA256 = "0f50d294641ebe7ec495a68d8e915bb0c1628798484261d40f46f4292b957f75"
SOURCE_SHA256 = {
    "whaleFrames": "579ff09b132864b9a9a7c7c3e38ef445b77850d9e3f7674eeabf34444c4c3b3c",
    "whaleLayers": "be2dda03c6fea8f85a1bb824e1022380b76758987010fe9ce7ab3f6802afddb6",
    "whaleIdle": "9310e69833d2abb8c18cd6ba6319d29d60835abe9f0616a4ee355725a8f72368",
}
THEME_SOURCE_SHA256 = "318349fed19dd8ed673ea3c7520de43bf7777bbcec3ddb1c79bba2da3d32f377"
PALETTE_SHA256 = {
    "dark": "5aa866d5b3657a83e01b185d98312d787dc01be02cd5906a9996730b05e35721",
    "light": "0f3061d79b36085ce64156c592f51afd9a5bc2b144bee98156ab80aa5f7f3bcc",
}
FRAME_NAMES = [
    "standard", "blink", "fin1", "fin2", "spout1", "spout2", "spout3",
    "spout4", "spout5", "spout6", "tail1", "tail2", "tail3", "tail4",
    "heart1", "heart2", "heart3", "sleep1", "sleep2", "sleep3", "sleep4",
    "sleep5",
]
OPENING = {
    "classic": [
        ("standard", 400), ("blink", 250), ("standard", 300),
        ("spout1", 150), ("spout2", 150), ("spout3", 150),
        ("spout4", 150), ("spout5", 150), ("spout6", 150),
        ("standard", 250), ("tail1", 150), ("tail2", 150),
        ("tail3", 150), ("tail4", 180), ("tail3", 150),
        ("tail2", 150), ("tail1", 150), ("standard", 300),
    ],
    "heart": [
        ("standard", 400), ("heart1", 350), ("heart2", 350),
        ("heart3", 450), ("heart2", 350), ("heart1", 350),
        ("standard", 500),
    ],
    "sleep": [
        ("standard", 400), ("sleep1", 250), ("sleep2", 250),
        ("sleep3", 250), ("sleep4", 250), ("sleep5", 300),
        ("sleep4", 250), ("sleep3", 250), ("sleep2", 250),
        ("sleep1", 250), ("standard", 400),
    ],
}
THEME_KEYS = [
    "autoAccept", "bashBorder", "accent", "toolNameMutate", "toolNameExec",
    "accentShimmer", "activity", "activityShimmer", "permission",
    "permissionShimmer", "planMode", "ide", "promptBorder",
    "promptBorderShimmer", "text", "inverseText", "inactive",
    "inactiveShimmer", "subtle", "suggestion", "remember", "background",
    "success", "error", "warning", "merged", "warningShimmer", "diffAdded",
    "diffRemoved", "diffAddedDimmed", "diffRemovedDimmed", "diffAddedWord",
    "diffRemovedWord", "toolCardBackground", "toolCardBackgroundDim",
    "toolDotExec", "toolDotRead", "toolDotWrite", "toolDotWeb", "toolDotTask",
    "syntaxKeyword", "syntaxString", "syntaxComment", "syntaxNumber",
    "syntaxFunction", "syntaxType", "syntaxVariable", "syntaxOperator",
    "syntaxPunctuation", "syntaxConstant", "professionalBlue", "chromeYellow",
    "contextBarSystem", "contextBarPrompt", "contextBarAssistant",
    "contextBarThinking", "contextBarTools", "ignition", "ignitionDim",
    "cursor", "mascotBody", "inputBackground", "userMessageBackground",
    "userMessageBackgroundHover", "messageActionsBackground", "selectionBg",
    "bashMessageBackgroundColor", "memoryBackgroundColor", "rate_limit_fill",
    "rate_limit_empty", "fastMode", "fastModeShimmer", "userPromptLabel",
    "subagentBullet", "subagentDescription", "subagentModel", "subagentElapsed",
    "subagentToolName", "subagentStatusRunning", "subagentStatusCompleted",
    "subagentStatusFailed",
]

passed = 0


def need(condition: bool, message: str) -> None:
    global passed
    if not condition:
        raise AssertionError(message)
    passed += 1


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def load_pinned_json(name: str, expected_sha256: str) -> dict:
    data = (ROOT / name).read_bytes()
    need(b"\r" not in data, f"{name}: expected LF bytes")
    actual = sha256(data)
    need(actual == expected_sha256, f"{name}: manifest sha256 {actual}, expected {expected_sha256}")
    value = json.loads(data)
    need(isinstance(value, dict), f"{name}: root must be an object")
    return value


def verify_frames(manifest: dict) -> None:
    need(manifest["schema_version"] == 1, "frames schema_version")
    need(manifest["upstream"]["repository"] == "https://github.com/ccch1mneyyy/dsh-TUI", "frames repository")
    need(manifest["upstream"]["commit"] == COMMIT, "frames upstream commit")
    need(manifest["upstream"]["license"] == "MIT", "frames license")
    for name, expected in SOURCE_SHA256.items():
        need(manifest["upstream"]["sources"][name]["sha256"] == expected, f"{name} source sha256")
    sprite = manifest["sprite"]
    need((sprite["width"], sprite["height"]) == (40, 25), "sprite dimensions")
    need(sprite["alphabet"] == ".DBLWHZ", "sprite alphabet")
    need(sprite["encoding"] == "ASCII" and sprite["line_endings"] == "LF", "sprite encoding")
    need(sprite["trailing_newline"] is True, "sprite trailing newline")

    entries = manifest["frames"]
    need([entry["name"] for entry in entries] == FRAME_NAMES, "frame names/source order")
    expected_files = [f"{name}.txt" for name in FRAME_NAMES]
    actual_files = sorted(path.name for path in (ROOT / "whale").glob("*.txt"))
    need(actual_files == sorted(expected_files), "whale directory must contain exactly the 22 declared frames")
    for index, (name, entry) in enumerate(zip(FRAME_NAMES, entries)):
        need(entry["index"] == index, f"{name}: source index")
        need(entry["file"] == f"whale/{name}.txt", f"{name}: path")
        need(isinstance(entry["source_line"], int) and entry["source_line"] > 0, f"{name}: source line")
        data = (ROOT / entry["file"]).read_bytes()
        need(len(data) == 1025 and data.count(b"\n") == 25, f"{name}: byte/line count")
        need(data.endswith(b"\n") and b"\r" not in data, f"{name}: LF and trailing newline")
        rows = data[:-1].decode("ascii").split("\n")
        need(len(rows) == 25 and all(len(row) == 40 for row in rows), f"{name}: 25x40")
        need(set("".join(rows)) <= set(".DBLWHZ"), f"{name}: invalid palette character")
        need(sha256(data) == entry["sha256"], f"{name}: frame sha256")

    opening = manifest["opening"]
    need(opening["source_symbol"] == "OPENING_SEQUENCES", "opening source symbol")
    need(opening["selection"]["ids"] == ["classic", "heart", "sleep"], "opening random id order")
    need(opening["loop"] is False, "opening must not loop")
    need(list(opening["sequences"]) == ["classic", "heart", "sleep"], "opening sequence order")
    for name, expected_steps in OPENING.items():
        sequence = opening["sequences"][name]
        actual_steps = [(step["frame"], step["ms"]) for step in sequence["steps"]]
        need(actual_steps == expected_steps, f"opening {name}: frame order/dwell")
        need(sequence["loop"] is False and sequence["settle_frame"] == "standard", f"opening {name}: settle/loop")
        need(sequence["total_ms"] == sum(ms for _, ms in expected_steps), f"opening {name}: total_ms")
        for index, step in enumerate(sequence["steps"]):
            need(step["step"] == index, f"opening {name}: step index {index}")
            need(step["frame_index"] == FRAME_NAMES.index(step["frame"]), f"opening {name}: frame index {index}")

    idle = manifest["idle"]
    need(idle["tick_ms"] == 120 and idle["minimum_scheduler_delay_ms"] == 16, "idle tick/scheduler delay")
    need(idle["layer_paint_order"] == ["eyes", "tail", "fin", "spout", "heart", "sleep"], "idle layer order")
    need(idle["resting_pose"] == {"tail": 0, "fin": 0, "spout": 0, "heart": 0, "sleep": 0, "blink": False}, "idle resting pose")
    need(idle["layer_frame_maps"] == {
        "tail": ["standard", "tail1", "tail2", "tail3", "tail4"],
        "fin": ["standard", "fin1", "fin2"],
        "spout": ["standard", "spout1", "spout2", "spout3", "spout4", "spout5", "spout6"],
        "heart": ["standard", "heart1", "heart2", "heart3"],
        "sleep": ["standard", "sleep1", "sleep2", "sleep3", "sleep4", "sleep5"],
        "eyes": {"awake": "standard", "blink": "blink"},
    }, "idle layer-to-frame maps")
    layers = idle["layers"]
    need(layers["tail"]["frame_order"] == ["tail1", "tail2", "tail3", "tail4", "tail3", "tail2", "tail1"], "idle tail order")
    need(layers["tail"]["working"] == {"mode": "continuous", "hold_ms": 240, "loop": True}, "idle working tail cadence")
    need(layers["tail"]["idle"] == {"mode": "occasional pass then layer 0", "hold_ms": 360, "gap_ms": 3000, "loop": False, "passes_repeat": True}, "idle tail cadence")
    need(layers["fin"]["frame_order"] == ["fin1", "fin2", "fin1"], "idle fin order")
    need(layers["fin"]["working"] == {"mode": "continuous", "hold_ms": 240, "loop": True}, "idle working fin cadence")
    need(layers["fin"]["idle"] == {"mode": "occasional pass then layer 0", "hold_ms": 240, "gap_ms": 1920, "loop": False, "passes_repeat": True}, "idle fin cadence")
    need(layers["tail"]["asleep"] == layers["fin"]["asleep"] == "same cadence as idle", "sleeping body layers keep idle cadence")
    need(layers["spout"] == {"idle_layer": 0, "frames": [], "loop": False}, "idle spout disabled")
    need(layers["heart"]["frame_order"] == ["heart1", "heart2", "heart3"], "idle heart order")
    need(layers["heart"]["hold_ms"] == 360 and layers["heart"]["loop"] is False, "idle heart cadence/one-shot")
    need(layers["heart"]["returns_to_layer"] == 0 and layers["heart"]["overlays_other_layers"] is True, "idle heart composition")
    need(layers["sleep"]["inactivity_delay_ms"] == 10000, "idle sleep delay")
    need(layers["sleep"]["entry"] == {"layer": 0, "frame": "standard", "hold_ms": 360}, "idle sleep entry")
    need(layers["sleep"]["frame_order"] == ["sleep1", "sleep2", "sleep3", "sleep4", "sleep5"], "idle sleep order")
    need(layers["sleep"]["hold_ms"] == 360 and layers["sleep"]["loop"] is True, "idle sleep cadence/loop")
    need(layers["sleep"]["wrap"] == "sleep5 -> sleep1; never wraps to standard", "idle sleep wrap")
    need(layers["sleep"]["sticky_while_idle"] is True and layers["sleep"]["clear_on"] == ["working", "heart/click"], "idle sleep sticky/wake")
    need(layers["blink"] == {
        "frame_order": ["blink"], "hold_ms": 120,
        "gap_ms": {"working": 1680, "idle": 5040},
        "loop": True, "overlays_other_layers": True,
    }, "idle blink cadence/loop")

    legacy_record = manifest["legacy_whale_standard"]
    legacy = (ROOT / "whale_standard.txt").read_bytes()
    standard = (ROOT / "whale" / "standard.txt").read_bytes()
    need(legacy_record["actual_sha256"] == sha256(legacy), "legacy standard recorded sha256")
    need(legacy_record["upstream_sha256"] == sha256(standard), "legacy upstream recorded sha256")
    need(legacy_record["matches_upstream"] is (legacy == standard), "legacy comparison result")
    legacy_rows = legacy.decode("ascii").splitlines()
    standard_rows = standard.decode("ascii").splitlines()
    differences = [
        {"row": row + 1, "column": column + 1, "legacy": legacy_rows[row][column], "upstream": standard_rows[row][column]}
        for row in range(25) for column in range(40)
        if legacy_rows[row][column] != standard_rows[row][column]
    ]
    need(legacy_record["difference_coordinates"] == differences, "legacy difference coordinates")


def verify_theme(manifest: dict) -> None:
    need(manifest["schema_version"] == 1, "theme schema_version")
    need(manifest["family"] == "Gentle Mist Blue" and manifest["family_zh"] == "雾蓝", "theme family")
    need(manifest["upstream"]["commit"] == COMMIT, "theme upstream commit")
    need(manifest["upstream"]["license"] == "MIT", "theme license")
    need(manifest["upstream"]["source"]["path"] == "src/theme.ts", "theme source path")
    need(manifest["upstream"]["source"]["sha256"] == THEME_SOURCE_SHA256, "theme source sha256")
    need(manifest["key_order"] == THEME_KEYS, "81 Theme keys/order")
    need(list(manifest["themes"]) == ["dark", "light"], "only Gentle Mist Blue dark/light")
    for theme_name, symbol in (("dark", "darkTheme"), ("light", "lightTheme")):
        palette = manifest["themes"][theme_name]
        need(palette["source_symbol"] == symbol, f"{theme_name}: source symbol")
        need(palette["color_count"] == 81, f"{theme_name}: color count")
        colors = palette["colors"]
        need(list(colors) == THEME_KEYS, f"{theme_name}: color keys/order")
        canonical_values = json.dumps({key: colors[key]["value"] for key in THEME_KEYS}, separators=(",", ":")).encode("utf-8")
        need(sha256(canonical_values) == PALETTE_SHA256[theme_name], f"{theme_name}: pinned palette sha256")
        need(palette["palette_sha256"] == PALETTE_SHA256[theme_name], f"{theme_name}: recorded palette sha256")
        for key, color in colors.items():
            value = color["value"]
            hex_value = color["hex"]
            if hex_value is None:
                need(value == "" and color["source_expression"] == "''", f"{theme_name}.{key}: empty color")
            else:
                need(re.fullmatch(r"#[0-9A-F]{6}", hex_value) is not None, f"{theme_name}.{key}: source hex")
                expected_value = "rgb({},{},{})".format(
                    int(hex_value[1:3], 16), int(hex_value[3:5], 16), int(hex_value[5:7], 16)
                )
                need(value == expected_value, f"{theme_name}.{key}: rgb conversion")
                need(color["source_expression"] == f"rgb('{hex_value}')", f"{theme_name}.{key}: source expression")
            need(color["sha256"] == sha256(value.encode("utf-8")), f"{theme_name}.{key}: color sha256")
            expected_owner = "MIST_BAR_RAMP" if key.startswith("contextBar") else symbol
            need(color["source"] == f"{expected_owner}.{key}", f"{theme_name}.{key}: source owner")
            need(isinstance(color["source_line"], int) and color["source_line"] > 0, f"{theme_name}.{key}: source line")


def main() -> int:
    global passed
    passed = 0
    try:
        frames = load_pinned_json("frames.json", FRAMES_JSON_SHA256)
        theme = load_pinned_json("theme-mist.json", THEME_JSON_SHA256)
        verify_frames(frames)
        verify_theme(theme)
    except (AssertionError, KeyError, IndexError, TypeError, ValueError, OSError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        print(f"OK={passed} FAIL=1", file=sys.stderr)
        return 1
    print("frames=22, size=25x40, opening=classic:18/3480ms heart:7/2750ms sleep:11/3100ms")
    print("idle=tail/fin/heart/sleep/blink cadence and loops pinned; theme=Gentle Mist Blue dark/light 81 colors each")
    print(f"OK={passed} FAIL=0")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
