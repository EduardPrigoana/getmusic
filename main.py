import re
import requests
from flask import Flask, jsonify, request, redirect
from flask_cors import CORS
import os

app = Flask(__name__)
CORS(app)

@app.route('/')
@app.route('/index')
@app.route('/index.html')
def redirect_to_prigoana():
    return redirect('https://prigoana.com/', code=302)

@app.route('/search')
def search():
    query = request.args.get('q')
    if not query:
        return jsonify({'error': 'Missing query parameter q'}), 400

    formatted_query = query.replace('+', ' ').replace('%26', '&').strip()

    lastfm_prefix = 'https://www.last.fm/music/'
    if formatted_query.startswith(lastfm_prefix):
        formatted_query = formatted_query[len(lastfm_prefix):]

    formatted_query = re.split(r'\s+\(?(feat|ft)\b', formatted_query, flags=re.IGNORECASE)[0].strip()

    search_url = f'https://dab.yeet.su/api/search?q={formatted_query}&offset=0&type=track'
    try:
        search_response = requests.get(search_url)
        search_response.raise_for_status()
    except requests.RequestException:
        return jsonify({'error': 'Failed to fetch track data'}), 500

    search_data = search_response.json()
    if not search_data.get('tracks'):
        return jsonify({'error': 'No tracks found'}), 404

    track = search_data['tracks'][0]
    track_id = track['id']

    stream_url = f'https://dab.yeet.su/api/stream?trackId={track_id}&quality=27'
    try:
        stream_response = requests.get(stream_url)
        stream_response.raise_for_status()
    except requests.RequestException:
        return jsonify({'error': 'Failed to fetch stream data'}), 500

    stream_data = stream_response.json()
    final_stream_url = stream_data.get('url')
    if not final_stream_url:
        return jsonify({'error': 'No stream URL found'}), 404

    return jsonify({'stream_url': final_stream_url})


if __name__ == '__main__':
    port = int(os.environ.get('PORT', 5000))  # Use Render's assigned port or default to 5000
    app.run(host='0.0.0.0', port=port)
