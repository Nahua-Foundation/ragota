import axios from 'axios';

const GATEWAY_URL = 'http://gateway:8080/api/v1/orders';

// submitOrder sends the order to the gateway service.
export async function submitOrder(userId: string, amount: number) {
  const response = await axios.post(GATEWAY_URL, {
    user_id: userId,
    amount: amount,
  });
  return response.data;
}

// fetchOrder loads an order from the gateway by id.
export async function fetchOrder(orderId: string) {
  const response = await axios.get(`http://gateway:8080/api/v1/orders/${orderId}`);
  return response.data;
}
