from flask import Flask, jsonify, abort, Response
from flask_cors import CORS
from werkzeug.exceptions import HTTPException
import requests
import re
from json import JSONDecodeError

SEARCH_URL_TEMPLATE = "https://eu.qobuz.squid.wtf/api/get-music"
DOWNLOAD_URL_TEMPLATE = "https://us.qobuz.squid.wtf/api/download-music"
VALID_QUALITIES = {5, 6, 7, 27}
DEFAULT_QUALITY = 27
REQUEST_TIMEOUT = 10

app = Flask(__name__)
CORS(app)

@app.errorhandler(HTTPException)
def handle_http_exception(e):
    response = e.get_response()
    response.data = jsonify({
        "error": {
            "code": e.code,
            "name": e.name,
            "description": e.description,
        }
    }).data
    response.content_type = "application/json"
    return response

def find_first_id_with_isrc(obj):
    if isinstance(obj, dict):
        track_id = obj.get('id')
        isrc = obj.get('isrc')
        if track_id and isrc and isinstance(isrc, str) and isrc.strip():
            return track_id
        for v in obj.values():
            found = find_first_id_with_isrc(v)
            if found is not None:
                return found
    elif isinstance(obj, list):
        for item in obj:
            found = find_first_id_with_isrc(item)
            if found is not None:
                return found
    return None

def perform_search(query: str, quality: int) -> Response:
    if quality not in VALID_QUALITIES:
        abort(400, description=f"Invalid quality value. Must be one of {list(VALID_QUALITIES)}.")

    stripped_query = re.split(r'\b(feat|ft)\b', query, flags=re.IGNORECASE)[0].strip()
    if not stripped_query:
        abort(400, description="Search query is empty after cleaning.")

    try:
        search_params = {'q': stripped_query, 'offset': 0}
        search_resp = requests.get(SEARCH_URL_TEMPLATE, params=search_params, timeout=REQUEST_TIMEOUT)
        search_resp.raise_for_status()
        search_data = search_resp.json()
    except requests.exceptions.RequestException:
        abort(503, description="The external search service is unavailable.")
    except JSONDecodeError:
        abort(500, description="Received an invalid response from the search service.")

    track_id = find_first_id_with_isrc(search_data)
    if track_id is None:
        abort(404, description="No track with a valid ID and ISRC was found.")

    try:
        download_params = {'track_id': track_id, 'quality': quality}
        download_resp = requests.get(DOWNLOAD_URL_TEMPLATE, params=download_params, timeout=REQUEST_TIMEOUT)
        download_resp.raise_for_status()
        download_data = download_resp.json()
    except requests.exceptions.RequestException:
        abort(503, description="The external download service is unavailable.")
    except JSONDecodeError:
        abort(500, description="Received an invalid response from the download service.")

    url = download_data.get('data', {}).get('url')
    if url:
        return jsonify({"url": url})
    else:
        abort(500, description="Download URL not found in the final API response.")

@app.route('/search/<path:query>', defaults={'quality': DEFAULT_QUALITY})
@app.route('/search/<path:query>/quality/<int:quality>')
def search(query: str, quality: int) -> Response:
    return perform_search(query, quality)

if __name__ == '__main__':
    app.run(host='0.0.0.0', debug=True)