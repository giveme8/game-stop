extends SceneTree

const SDK = preload("../sdk/godot/game_stop.gd")
const Example = preload("../sdk/godot/example.gd")
var checks := 0
var failures := 0


class CaptureSDK:
	extends "../sdk/godot/game_stop.gd"
	var captured: Array = []
	func _request(snapshot: Dictionary, suffix: String, method: int, body: String = "") -> Dictionary:
		captured.append({"config": snapshot, "suffix": suffix, "method": method, "body": body})
		await get_tree().process_frame
		return _response(true, 200, {}, "")


func _initialize() -> void:
	_run.call_deferred()


func check(condition: bool, label: String) -> void:
	checks += 1
	if not condition:
		failures += 1
		push_error("FAIL: " + label)


func _run() -> void:
	var sdk := SDK.new()
	root.add_child(sdk)
	var example := Example.new()
	check(example is Node, "example really parses")
	example.free()
	for origin in ["http://example.com", "https://user:password@example.com", "https://example.com/path", "https://example.com?token=x", "https://example.com#x", "https://example.com:99999", "https://example.com\r\nX: y"]:
		check(not sdk.configure(origin, "demo").ok, "reject unsafe origin")
	for game in ["../game", "A", "", "a/b", "a".repeat(65)]:
		check(not sdk.configure("https://example.com", game).ok, "reject game ID")
	for token in ["gs_" + "a".repeat(43), "Bearer admin", "ps_x\r\nX: y", "ps_" + "a".repeat(43) + "\n"]:
		check(not sdk.configure("https://example.com", "demo", token).ok, "reject non-play credential")
	check(sdk.configure("https://example.com/", "demo").ok, "anonymous config")
	sdk._in_flight = SDK.MAX_IN_FLIGHT
	check((await sdk.get_leaderboard()).error == "too_many_requests", "bounded concurrency")
	sdk._in_flight = 0
	var detached := SDK.new()
	detached.configure("https://example.com", "demo")
	check((await detached.get_leaderboard()).error == "node_not_in_tree", "detached node fails safely")
	detached.free()
	check((await sdk.submit_score(3)).error == "play_token_required", "anonymous cannot submit")
	for value in [1.0, true, "1", null, NAN, INF, 9007199254740992, -9007199254740992]:
		check((await sdk.submit_score(value)).error == "invalid_safe_integer", "safe integer validation")
	check((await sdk.submit_score(-1, "time-ms")).error == "invalid_time", "negative time")
	check((await sdk.submit_score(0, "time-ms")).error == "invalid_time", "zero time")
	check((await sdk.submit_score(-1)).error == "invalid_score", "negative score")
	check((await sdk.submit_score(1, "other")).error == "invalid_board", "invalid submit board")
	check((await sdk.submit_score(1, "score", "../id")).error == "invalid_submission_id", "invalid ID")
	for limit in [0, 101, 1.0, true, "20"]:
		check((await sdk.get_leaderboard("score", limit)).error == "invalid_limit", "invalid limit")
	check((await sdk.get_leaderboard("other")).error == "invalid_board", "invalid read board")
	var ids := {}
	for i in range(128):
		var id := SDK.new_submission_id()
		check(id.length() == 32 and not ids.has(id), "random ID shape/uniqueness")
		ids[id] = true
	var capture := CaptureSDK.new()
	root.add_child(capture)
	capture.configure("https://example.com", "public-game")
	check((await capture.get_leaderboard()).ok, "anonymous read allowed")
	check(capture.captured[0].config.token == "", "no anonymous credential")
	check(capture.captured[0].suffix == "/leaderboard?board=score&limit=20", "GET defaults")
	var play_token := "ps_" + "a".repeat(43)
	check(capture.configure("https://example.com", "game-a", play_token).ok, "play config")
	_pending_submit(capture)
	capture.configure("https://other.example.com", "game-b")
	await process_frame
	var request: Dictionary = capture.captured[1]
	check(request.config.game_id == "game-a" and request.config.token == play_token and request.config.origin == "https://example.com", "in-flight immutable configuration")
	check(request.method == HTTPClient.METHOD_POST and request.suffix == "/scores", "POST route")
	check(request.body.contains('"submissionId":"retry-id-00000001"') and request.body.contains('"value":9007199254740991'), "exact safe integer JSON and retry key")
	check(not capture.configure("http://bad", "demo").ok and capture._config.is_empty(), "invalid reconfigure clears token")
	check((await capture.get_leaderboard()).error == "not_configured", "fail closed after config error")
	check(sdk._decode_response(0, 200, '{"items":[]}'.to_utf8_buffer()).ok, "decode success")
	check(sdk._decode_response(0, 403, '{"error":"forbidden"}'.to_utf8_buffer()).status == 403, "HTTP status retained")
	check(sdk._decode_response(0, 302, PackedByteArray()).error == "redirect_forbidden", "redirect denied")
	check(sdk._decode_response(0, 200, 'not json'.to_utf8_buffer()).error == "invalid_json", "invalid JSON")
	check(sdk._decode_response(0, 200, 'null'.to_utf8_buffer()).error == "invalid_json_shape", "JSON shape")
	check(not sdk._decode_response(HTTPRequest.RESULT_TIMEOUT, 0, PackedByteArray()).ok, "timeout fails")
	check(sdk._decode_response(HTTPRequest.RESULT_BODY_SIZE_LIMIT_EXCEEDED, 200, PackedByteArray()).error == "response_too_large", "body limit fails")
	var oversized := PackedByteArray()
	oversized.resize(SDK.RESPONSE_BODY_LIMIT + 1)
	check(sdk._decode_response(0, 200, oversized).error == "response_too_large", "defensive decoded body limit")
	ProjectSettings.set_setting("audio/general/text_to_speech", false)
	check(not sdk.speak("a".repeat(2001)) and sdk.last_tts_error == "invalid_text", "TTS text cap")
	check(not sdk.speak("hello", "") and sdk.last_tts_error == "invalid_language", "TTS language required")
	check(not sdk.speak("hello", "z".repeat(36)) and sdk.last_tts_error == "invalid_language", "TTS language cap")
	check(not sdk.speak("hello") and sdk.last_tts_error == "unsupported:tts_disabled", "disabled TTS")
	check(sdk._voice_status(PackedStringArray()).error == "unsupported:no_voices", "empty voices unsupported")
	ProjectSettings.set_setting("audio/general/text_to_speech", true)
	# With the headless display backend there must be no usable system voices.
	check(not sdk.speak("hello", "zz-NO-VOICE") and sdk.last_tts_error == "unsupported:no_voices", "real headless unsupported TTS")
	sdk.stop_speaking()
	ProjectSettings.set_setting("audio/general/text_to_speech", false)
	sdk.free()
	capture.free()
	print("platform_sdk_test: %d checks, %d failures" % [checks, failures])
	quit(1 if failures else 0)


func _pending_submit(sdk: Node) -> void:
	await sdk.submit_score(9007199254740991, "score", "retry-id-00000001")
