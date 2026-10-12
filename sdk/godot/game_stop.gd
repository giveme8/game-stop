class_name GameStopSDK
extends Node
## Instantiate from the game's scene code, not an autoload: the native frame does
## not load a mounted game's autoloads/settings. Configuration is memory-only.
## All network methods return {ok: bool, status: int, data: Variant, error: String}.
## Await submit_score/get_leaderboard. status=0 denotes a local/transport failure.
## A caller owns retries: retain new_submission_id() for the same logical result.
## Never pass a management token; this SDK has no registration/admin API.
## Platform integration must inject a short-lived ps_ play token into memory;
## the native frame is not modified and does not inject one automatically.

signal tts_failed(reason: String)

const MAX_SAFE_INTEGER := 9007199254740991
const RESPONSE_BODY_LIMIT := 1048576
const REQUEST_TIMEOUT_SECONDS := 15.0
const MAX_IN_FLIGHT := 8

var _config: Dictionary = {}
var _in_flight := 0
var last_tts_error := ""


func configure(base_url: String, game_id: String, play_token: String = "") -> Dictionary:
	# Fail closed, including clearing the old credential on an invalid replacement.
	_config = {}
	if not _matches("^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?/?$", base_url):
		return _failure("invalid_https_origin")
	var authority := base_url.trim_prefix("https://").trim_suffix("/")
	if ":" in authority:
		var port := int(authority.get_slice(":", 1))
		if port < 1 or port > 65535:
			return _failure("invalid_https_origin")
	if not _matches("^[a-z0-9][a-z0-9-]{0,63}$", game_id):
		return _failure("invalid_game_id")
	if not play_token.is_empty() and not _matches("^ps_[A-Za-z0-9_-]{43}$", play_token):
		return _failure("invalid_play_token")
	_config = {"origin": base_url.trim_suffix("/"), "game_id": game_id, "token": play_token}
	return _response(true, 0, null, "")


static func new_submission_id() -> String:
	# 128 bits from the OS CSPRNG; no clock/counter-based idempotency keys.
	return Crypto.new().generate_random_bytes(16).hex_encode()


func submit_score(value: Variant, board: String = "score", submission_id: String = "") -> Dictionary:
	if not _valid_board(board):
		return _failure("invalid_board")
	# Reject floats (including NaN/Inf and rounded large numbers), bools and strings.
	if typeof(value) != TYPE_INT or value < -MAX_SAFE_INTEGER or value > MAX_SAFE_INTEGER:
		return _failure("invalid_safe_integer")
	if board == "score" and value < 0:
		return _failure("invalid_score")
	if board == "time-ms" and value < 1:
		return _failure("invalid_time")
	if submission_id.is_empty():
		submission_id = new_submission_id()
	if not _matches("^[A-Za-z0-9_-]{16,64}$", submission_id):
		return _failure("invalid_submission_id")
	var snapshot := _config.duplicate()
	if snapshot.is_empty():
		return _failure("not_configured")
	if snapshot.token.is_empty():
		return _failure("play_token_required")
	var payload := JSON.stringify({"board": board, "value": value, "submissionId": submission_id})
	return await _request(snapshot, "/scores", HTTPClient.METHOD_POST, payload)


func get_leaderboard(board: String = "score", limit: Variant = 20) -> Dictionary:
	if not _valid_board(board):
		return _failure("invalid_board")
	if typeof(limit) != TYPE_INT or limit < 1 or limit > 100:
		return _failure("invalid_limit")
	var snapshot := _config.duplicate()
	if snapshot.is_empty():
		return _failure("not_configured")
	return await _request(snapshot, "/leaderboard?board=%s&limit=%d" % [board, limit], HTTPClient.METHOD_GET)


func _request(snapshot: Dictionary, suffix: String, method: int, body: String = "") -> Dictionary:
	if not is_inside_tree():
		return _failure("node_not_in_tree")
	if _in_flight >= MAX_IN_FLIGHT:
		return _failure("too_many_requests")
	# Each call gets its own request AND frozen origin/game/token. configure() can
	# run while this coroutine awaits without redirecting or relabeling credentials.
	var request := HTTPRequest.new()
	request.timeout = REQUEST_TIMEOUT_SECONDS
	request.body_size_limit = RESPONSE_BODY_LIMIT
	request.max_redirects = 0
	request.accept_gzip = false
	add_child(request)
	var headers := PackedStringArray(["Accept: application/json"])
	if method == HTTPClient.METHOD_POST and not snapshot.token.is_empty():
		headers.append("X-Play-Token: " + snapshot.token)
	if method == HTTPClient.METHOD_POST:
		headers.append("Content-Type: application/json")
	var url: String = snapshot.origin + "/api/v1/games/" + snapshot.game_id + suffix
	_in_flight += 1
	var start_error := request.request(url, headers, method, body)
	if start_error != OK:
		_in_flight -= 1
		request.queue_free()
		return _failure("request_start_failed:%d" % start_error)
	var result: Array = await request.request_completed
	_in_flight -= 1
	request.queue_free()
	return _decode_response(result[0], result[1], result[3])


func _decode_response(result: int, status: int, body: PackedByteArray) -> Dictionary:
	if status >= 300 and status < 400:
		return _response(false, status, null, "redirect_forbidden")
	if body.size() > RESPONSE_BODY_LIMIT or result == HTTPRequest.RESULT_BODY_SIZE_LIMIT_EXCEEDED:
		return _response(false, status, null, "response_too_large")
	if result != HTTPRequest.RESULT_SUCCESS:
		return _response(false, status, null, "transport_failed:%d" % result)
	var parser := JSON.new()
	if parser.parse(body.get_string_from_utf8()) != OK:
		return _response(false, status, null, "invalid_json")
	if not (parser.data is Dictionary or parser.data is Array):
		return _response(false, status, null, "invalid_json_shape")
	if status < 200 or status >= 300:
		return _response(false, status, parser.data, "http_error:%d" % status)
	return _response(true, status, parser.data, "")


func tts_status(language: String = "zh") -> Dictionary:
	if not ProjectSettings.get_setting("audio/general/text_to_speech", false):
		return {"supported": false, "error": "unsupported:tts_disabled", "voices": PackedStringArray()}
	# No OHOS assumption: only the currently running engine/backend can answer.
	var voices := DisplayServer.tts_get_voices_for_language(language)
	return _voice_status(voices)


func _voice_status(voices: PackedStringArray) -> Dictionary:
	return {"supported": not voices.is_empty(), "error": "unsupported:no_voices" if voices.is_empty() else "", "voices": voices}


func speak(text: String, language: String = "zh") -> bool:
	last_tts_error = ""
	if text.strip_edges().is_empty() or text.length() > 2000:
		last_tts_error = "invalid_text"
	elif language.strip_edges().is_empty() or language.length() > 35:
		last_tts_error = "invalid_language"
	else:
		var capability := tts_status(language)
		if not capability.supported:
			last_tts_error = capability.error
		else:
			# Godot's method returns void: true means handed to an available voice,
			# NOT proof of audible output or completion. DisplayServer callbacks are
			# global; deliberately do not overwrite another game's callback handlers.
			DisplayServer.tts_speak(text, capability.voices[0])
			return true
	tts_failed.emit(last_tts_error)
	return false


func stop_speaking() -> void:
	# DisplayServer TTS is process-wide; this also stops other callers' utterances.
	if ProjectSettings.get_setting("audio/general/text_to_speech", false):
		DisplayServer.tts_stop()


static func _matches(pattern: String, value: String) -> bool:
	var regex := RegEx.new()
	regex.compile(pattern)
	var matched := regex.search(value)
	return matched != null and matched.get_string() == value


static func _valid_board(board: String) -> bool:
	return board == "score" or board == "time-ms"


static func _failure(error: String) -> Dictionary:
	return _response(false, 0, null, error)


static func _response(ok: bool, status: int, data: Variant, error: String) -> Dictionary:
	return {"ok": ok, "status": status, "data": data, "error": error}
