// Function triggered by the bucket (uploads under products/, *.jpg): records the new photo
// on its product through the API, over the private network.
//   trigger: bucket "photos", event "created", prefix "products/", suffix ".jpg"
export const handler = async (event) => {
  const results = [];
  for (const r of event.Records || []) {
    const key = decodeURIComponent(r.s3.object.key);
    const res = await fetch(`${process.env.API_URL || "http://api.shop.internal"}/api/internal/image`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Internal-Token": process.env.INTERNAL_TOKEN || "" },
      body: JSON.stringify({ key }),
    });
    console.log("photo", key, "->", res.status);
    results.push({ key, status: res.status });
  }
  return { results };
};
