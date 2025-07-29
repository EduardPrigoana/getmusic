import re
import logging
from flask import Flask, jsonify, request, redirect
from flask_cors import CORS
import os
import cloudscraper

app = Flask(__name__)
CORS(app)

# Set up logging to stdout with DEBUG level
logging.basicConfig(level=logging.DEBUG, format='%(asctime)s %(levelname)s %(message)s')

scraper = cloudscraper.create_scraper()  # cloudscraper session to bypass Cloudflare

@app.before_request
def log_request_info():
    logging.debug(f"Incoming request: {request.method} {request.url}")
    logging.debug(f"Headers: {dict(request.headers)}")
    if request.method == "POST":
        logging.debug(f"Body: {request.get_data()}")

@app.route('/')
@app.route('/index')
@app.route('/index.html')
def redirect_to_prigoana():
    logging.info("Redirecting to https://prigoana.com/")
    return redirect('https://prigoana.com/', code=302)

@app.route('/search')
def search():
    query = request.args.get('q')
    logging.debug(f"Search query param q: {query}")

    if not query:
        logging.warning("Missing query parameter q")
        return jsonify({'error': 'Missing query parameter q'}), 400

    formatted_query = query.replace('+', ' ').replace('%26', '&').strip()
    logging.debug(f"Formatted query after replacements: {formatted_query}")

    lastfm_prefix = 'https://www.last.fm/music/'
    if formatted_query.startswith(lastfm_prefix):
        formatted_query = formatted_query[len(lastfm_prefix):]
        logging.debug(f"Formatted query after removing last.fm prefix: {formatted_query}")

    # Remove (feat ...) or (ft ...) suffixes
    formatted_query = re.split(r'\s+\(?(feat|ft)\b', formatted_query, flags=re.IGNORECASE)[0].strip()
    logging.debug(f"Formatted query after removing featuring info: {formatted_query}")

    search_url = f'https://dab.yeet.su/api/search?q={formatted_query}&offset=0&type=track'
    logging.info(f"Making search request to: {search_url}")

    try:
        search_response = scraper.get(search_url)
        logging.debug(f"Search response status: {search_response.status_code}")
        logging.debug(f"Search response headers: {dict(search_response.headers)}")
        logging.debug(f"Search response text: {search_response.text}")
        search_response.raise_for_status()
    except Exception as e:
        logging.error(f"Search request failed: {e}")
        return jsonify({'error': 'Failed to fetch track data'}), 500

    search_data = search_response.json()
    if not search_data.get('tracks'):
        logging.warning("No tracks found in search response")
        return jsonify({'error': 'No tracks found'}), 404

    track = search_data['tracks'][0]
    track_id = track['id']
    logging.debug(f"Selected track id: {track_id}")

    stream_url = f'https://dab.yeet.su/api/stream?trackId={track_id}&quality=27'
    logging.info(f"Making stream request to: {stream_url}")

    try:
        stream_response = scraper.get(stream_url)
        logging.debug(f"Stream response status: {stream_response.status_code}")
        logging.debug(f"Stream response headers: {dict(stream_response.headers)}")
        logging.debug(f"Stream response text: {stream_response.text}")
        stream_response.raise_for_status()
    except Exception as e:
        logging.error(f"Stream request failed: {e}")
        return jsonify({'error': 'Failed to fetch stream data'}), 500

    stream_data = stream_response.json()
    final_stream_url = stream_data.get('url')
    if not final_stream_url:
        logging.warning("No stream URL found in stream response")
        return jsonify({'error': 'No stream URL found'}), 404

    logging.info(f"Returning final stream URL: {final_stream_url}")
    return jsonify({'stream_url': final_stream_url})

if __name__ == '__main__':
    port = int(os.environ.get('PORT', 5000))  # Use Render's assigned port or default to 5000
    logging.info(f"Starting Flask app on port {port}")
    app.run(host='0.0.0.0', port=port)
