from flask import Flask, jsonify
from flask_cors import CORS
import requests
import re

app = Flask(__name__)
CORS(app)  # Enable CORS for all routes and origins

def find_first_id_with_isrc(obj):
    if isinstance(obj, dict):
        if 'id' in obj and 'isrc' in obj and obj['isrc'] and isinstance(obj['isrc'], str) and obj['isrc'].strip():
            return obj['id']
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

@app.route('/search/<path:query>')
def search(query):
    # Strip everything after "feat" (case-insensitive)
    stripped_query = re.split(r'\bfeat\b', query, flags=re.IGNORECASE)[0].strip()

    # URL encode spaces as %20
    query_encoded = stripped_query.replace('+', ' ').replace(' ', '%20')
    search_url = f"https://eu.qobuz.squid.wtf/api/get-music?q={query_encoded}&offset=0"

    try:
        search_resp = requests.get(search_url)
        search_resp.raise_for_status()
        search_data = search_resp.json()
    except Exception as e:
        return jsonify({"error": "Failed to fetch or parse search data", "details": str(e)}), 500

    track_id = find_first_id_with_isrc(search_data)
    if track_id is None:
        return jsonify({"error": "No track with valid 'id' and 'isrc' found"}), 404

    download_url = f"https://eu.qobuz.squid.wtf/api/download-music?track_id={track_id}&quality=27"

    try:
        download_resp = requests.get(download_url)
        download_resp.raise_for_status()
        download_data = download_resp.json()
    except Exception as e:
        return jsonify({"error": "Failed to fetch or parse download data", "details": str(e)}), 500

    url = download_data.get('data', {}).get('url')
    if url:
        return jsonify({"url": url})
    else:
        return jsonify({"error": "URL not found in download response"}), 500

if __name__ == '__main__':
    app.run(host='0.0.0.0', debug=True)
